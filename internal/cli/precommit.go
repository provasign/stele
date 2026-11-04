package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/specsync"
)

// cmdPrecommit is the git pre-commit hook `stele init` installs. It is
// silent unless the commit stages spec or registry files. It blocks (exit 1)
// only on what an approval clears: approved rules whose statement text has
// changed or disappeared, and a staged registry the gate refuses (conflicts,
// unapproved rules, errors), checked as CI checks it. A new requirement the
// owner has not approved gets the command that checks it, without blocking,
// except in a commit to a Shale project's base branch (onBase). Every
// message ends with the command to run, for the coding agent to act on.
func cmdPrecommit(args []string, out io.Writer) int {
	cfg, err := config.Find(".")
	if err != nil || cfg == nil {
		return 0 // not a Stele project, or one Stele cannot read: never block on that
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return 0
	}
	root = strings.TrimSpace(root)
	if reg, err := ir.LoadDir(cfg.RegistryDir()); err == nil {
		refreshCards(root, reg) // before the staged list is read: a refreshed card is staged
	}
	staged, err := git("diff", "--cached", "--name-only", "-z", "--diff-filter=ACMRD")
	if err != nil {
		return 0
	}
	files := splitNUL(staged)
	rel := func(p string) string {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return p
		}
		return filepath.ToSlash(r)
	}
	regRel := rel(cfg.RegistryDir())
	under := func(dir string) bool {
		return slices.ContainsFunc(files, func(f string) bool { return f == dir || strings.HasPrefix(f, dir+"/") })
	}
	specDirs := specDirsOf(cfg, root, rel)
	touchesReg := under(regRel)
	touchesSpecs := false
	for _, d := range specDirs {
		touchesSpecs = touchesSpecs || slices.ContainsFunc(files, func(f string) bool {
			return strings.HasPrefix(f, d+"/") && !strings.HasPrefix(f, regRel+"/")
		})
	}
	if !touchesReg && !touchesSpecs {
		return 0
	}
	next := nextCommand(cfg, root)
	reg, err := ir.LoadDir(cfg.RegistryDir())
	shale := cfg.Specs != nil && cfg.Specs.Format == "shale"
	if err != nil || len(reg.Rules) == 0 && !shale {
		// With no requirements yet, there is nothing to adopt. A Shale
		// project's requests are checked one by one as they are declared,
		// never adopted: its first ones are new requirements as any later.
		if touchesSpecs && hasRequirements(cfg, root) {
			fmt.Fprintf(out, "Stele: this project's specs are not adopted yet, so this commit is not checked.\nNext: run `stele adopt --json`. It reads every requirement once and checks them against each other; then ask the owner its questions and record the answers, as for `stele change`.\n")
		}
		return 0
	}

	var blocks, notes []string
	if touchesSpecs && cfg.Specs != nil {
		// What the commit carries is the index, not the working tree: a
		// change staged and then undone in the working tree would pass a
		// check of the files on disk (a code review, 2026-11-03).
		staged, cleanup, serr := stagedTree(root, cfg, rel)
		if serr != nil {
			fmt.Fprintf(out, "Stele: could not read the staged specs (%v); CI's stele gate still checks them.\n", serr)
			return 0
		}
		defer cleanup()
		if r, err := evaluateStaged(cfg, staged, rel); err == nil {
			for _, id := range r.StaleRules {
				blocks = append(blocks, fmt.Sprintf("approved rule %s was approved for text that has since changed", id))
			}
			// The owner retired a request (an update_old or exception answer)
			// whose replacement is not approved yet: its rules retire with
			// that approval. Blocked, the work could not be committed while
			// the replacement waited (a developer trial, 2026-11-03).
			retired := map[string]bool{}
			if shale {
				if rs, err := importer.RetiredIntents(staged); err == nil {
					for _, x := range rs {
						retired[x.ID] = true
					}
				}
			}
			waiting := map[string]bool{}
			for _, id := range r.OrphanRules {
				if x := reg.Rule(id); x != nil && retired[x.StatementID] {
					waiting[x.StatementID] = true
					continue
				}
				blocks = append(blocks, fmt.Sprintf("approved rule %s belongs to a requirement that is gone", id))
			}
			for _, id := range slices.Sorted(maps.Keys(waiting)) {
				notes = append(notes, fmt.Sprintf("the owner retired %s; its rules retire when the change that replaces it is approved", id))
			}
			ruled := map[string]bool{}
			for _, x := range reg.Rules {
				ruled[x.StatementID] = true
			}
			for _, s := range r.Pending {
				if !ruled[s.ID] {
					// Checked or not, it waits for the owner: "not checked
					// yet" misled an agent whose check was waiting for the
					// approval.
					notes = append(notes, fmt.Sprintf("new requirement %s is not approved yet: %s", s.ID, oneLine(s.Prose, 100)))
				}
			}
		}
	} else if touchesSpecs && !touchesReg {
		notes = append(notes, "spec files changed but the approved registry did not")
	}
	if touchesReg {
		res, skipped, err := gateStaged(root, regRel)
		switch {
		case err != nil:
			fmt.Fprintf(out, "Stele: could not check the staged registry (%v); CI's stele gate still will.\n", err)
		case skipped:
			// The registry's first commit (adoption): nothing approved to compare with.
		default:
			for _, e := range res.Errors {
				blocks = append(blocks, "registry error: "+e)
			}
			for _, id := range res.Unapproved {
				blocks = append(blocks, fmt.Sprintf("rule %s is not approved by the owner for its current text", id))
			}
			if res.Check != nil {
				for _, c := range res.Check.Conflicts {
					blocks = append(blocks, "requirements conflict: "+strings.Join(c.Rules, " + "))
				}
				for _, d := range res.Check.Dead {
					blocks = append(blocks, fmt.Sprintf("%s can never happen: %s", d.Response, strings.Join(d.Rules, " + ")))
				}
			}
		}
	}
	// On the branch changes merge into there is no later merge to wait
	// for: the commit is the merge, so what waits for the owner blocks it.
	// A developer trial (2026-11-03) committed unapproved requests to main
	// twice under a note that said "before this change merges".
	if len(blocks) == 0 && len(notes) > 0 && onBase(shale) {
		base := baseBranch()
		fmt.Fprintf(out, "Stele blocked this commit to %s: the owner has not approved the requests it carries.\n", base)
		for _, n := range notes {
			fmt.Fprintf(out, "- %s\n", n)
		}
		fmt.Fprintf(out, "Next: run `%s`, put its questions to the owner, ask for the approval, then commit again; or commit on a branch and merge it once approved. Do not bypass this check with --no-verify.\n", next)
		return 1
	}
	if len(blocks) == 0 {
		if len(notes) > 0 {
			fmt.Fprintf(out, "Stele: %s.\nNext: run `%s` and put its questions to the owner and ask for the approval before this change merges.\n", strings.Join(notes, "; "), next)
		}
		return 0
	}
	fmt.Fprintln(out, "Stele blocked this commit: the specs and the approved requirements disagree.")
	for _, b := range append(blocks, notes...) {
		fmt.Fprintf(out, "- %s\n", b)
	}
	fmt.Fprintf(out, "Next: run `%s`, ask the owner the questions it marks for them, record their answers as it says, then commit again. Do not bypass this check with --no-verify.\n", next)
	return 1
}

// specDirsOf names, relative to the repository root, where the project's
// spec files live: the configured root, else the standard folders of each
// format that exist.
func specDirsOf(cfg *config.Config, root string, rel func(string) string) []string {
	if cfg.Specs != nil {
		d := rel(cfg.SpecsDir())
		switch cfg.Specs.Format {
		case "openspec":
			return []string{strings.TrimPrefix(d+"/openspec", "./")}
		case "speckit":
			return []string{strings.TrimPrefix(d+"/specs", "./")}
		case "kiro":
			return []string{strings.TrimPrefix(d+"/.kiro/specs", "./")}
		case "shale":
			return []string{strings.TrimPrefix(d+"/.shale", "./")}
		}
		return []string{d}
	}
	var out []string
	for _, d := range []string{"openspec", "specs", ".kiro/specs"} {
		if fi, err := os.Stat(filepath.Join(root, d)); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// nextCommand is the command that checks this project's spec change.
func nextCommand(cfg *config.Config, root string) string {
	format := ""
	if cfg.Specs != nil {
		format = cfg.Specs.Format
	} else {
		for _, f := range []struct{ dir, format string }{{"openspec", "openspec"}, {".kiro/specs", "kiro"}, {".specify", "speckit"}} {
			if _, err := os.Stat(filepath.Join(root, f.dir)); err == nil {
				format = f.format
				break
			}
		}
	}
	switch format {
	case "openspec":
		return "stele change openspec . <change-id> --json` (list the changes with `stele change openspec . --list"
	case "speckit", "kiro", "shale":
		return "stele change " + format + " . --base " + baseBranch() + " --json"
	}
	return "stele watch --once --json"
}

// onBase reports whether HEAD is the branch changes merge into. Only a
// Shale project's requests are held there: they arrive with the code that
// carries them out, while spec drafts are committed before their approval.
func onBase(shale bool) bool {
	if !shale {
		return false
	}
	branch, err := git("symbolic-ref", "--short", "HEAD")
	return err == nil && strings.TrimSpace(branch) == baseBranch()
}

// baseBranch is the branch changes merge into: origin's default, else main.
func baseBranch() string {
	if ref, err := git("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(ref), "origin/")
	}
	return "main"
}

// stagedTree exports, into a temporary directory laid out as the
// repository, the staged spec files, snapshot, registry and retired
// requests: what the commit carries.
func stagedTree(root string, cfg *config.Config, rel func(string) string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "stele-staged-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	specs := rel(cfg.SpecsDir())
	if cfg.Specs.Format == "shale" {
		specs = filepath.ToSlash(filepath.Join(specs, ".shale"))
	}
	listed, err := git("ls-files", "-z", "--", specs, rel(cfg.SnapshotFile()), rel(cfg.RegistryDir()), importer.RetiredIntentsFile)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	var paths []string
	for _, p := range splitNUL(listed) {
		switch strings.ToLower(filepath.Ext(p)) {
		case ".md", ".yaml", ".yml", ".json", ".jsonl":
			paths = append(paths, p)
		default:
			if p == rel(cfg.SnapshotFile()) {
				paths = append(paths, p)
			}
		}
	}
	if len(paths) > 0 {
		export := exec.Command("git", append([]string{"checkout-index", "--prefix=" + tmp + "/", "--"}, paths...)...)
		export.Dir = root
		if out, err := export.CombinedOutput(); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("git checkout-index: %v: %s", err, out)
		}
	}
	return tmp, cleanup, nil
}

// evaluateStaged is evaluateSpecs over the staged tree at dir.
func evaluateStaged(cfg *config.Config, dir string, rel func(string) string) (*specsync.Result, error) {
	o := specsync.Options{Registry: filepath.Join(dir, rel(cfg.RegistryDir())), Root: filepath.Join(dir, rel(cfg.SpecsDir())), Snapshot: filepath.Join(dir, rel(cfg.SnapshotFile())), Format: cfg.Specs.Format, AnyCase: cfg.Specs.AnyCase}
	if err := os.MkdirAll(o.Root, 0o755); err != nil {
		return nil, err
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return specsync.Evaluate(o)
}

func evaluateSpecs(cfg *config.Config) (*specsync.Result, error) {
	o := specsync.Options{Registry: cfg.RegistryDir(), Root: cfg.SpecsDir(), Snapshot: cfg.SnapshotFile(), Format: cfg.Specs.Format, AnyCase: cfg.Specs.AnyCase}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return specsync.Evaluate(o)
}

// gateStaged gates the registry as staged against the one in HEAD. skipped
// is true when HEAD has no registry yet.
func gateStaged(root, regRel string) (*gate.Result, bool, error) {
	tmp, err := os.MkdirTemp("", "stele-precommit-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(tmp)
	base, head := filepath.Join(tmp, "base"), filepath.Join(tmp, "head")
	if _, err := git("cat-file", "-e", "HEAD:"+regRel); err != nil {
		return nil, true, nil
	}
	archive := exec.Command("git", "archive", "--format=tar", "HEAD", "--", regRel)
	archive.Dir = root
	tarball, err := archive.Output()
	if err != nil {
		return nil, false, fmt.Errorf("git archive: %w", err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, false, err
	}
	untar := exec.Command("tar", "-x", "-C", base)
	untar.Stdin = bytes.NewReader(tarball)
	if out, err := untar.CombinedOutput(); err != nil {
		return nil, false, fmt.Errorf("tar: %v: %s", err, out)
	}
	listed, err := git("ls-files", "-z", "--", regRel)
	if err != nil {
		return nil, false, err
	}
	paths := splitNUL(listed)
	if len(paths) == 0 {
		return &gate.Result{Accepted: true}, false, nil
	}
	export := exec.Command("git", append([]string{"checkout-index", "--prefix=" + head + "/", "--"}, paths...)...)
	export.Dir = root
	if out, err := export.CombinedOutput(); err != nil {
		return nil, false, fmt.Errorf("git checkout-index: %v: %s", err, out)
	}
	res, err := gate.Dirs(context.Background(), &solver.Z3{}, filepath.Join(base, regRel), filepath.Join(head, regRel))
	if err != nil {
		return nil, false, err
	}
	return res, false, nil
}

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return string(out), err
}

func splitNUL(s string) []string {
	var out []string
	for _, f := range strings.Split(s, "\x00") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// hasRequirements reports whether the project's specs hold any requirement;
// when they cannot be read, it says yes, so the adopt step is still shown.
func hasRequirements(cfg *config.Config, root string) bool {
	if cfg.Specs == nil {
		return true
	}
	stmts, err := importSpecs(root, cfg.Specs)
	return err != nil || len(stmts) > 0
}
