package change

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// openSpecChange reads an OpenSpec change folder and applies it to the
// current specs.
func openSpecChange(opt Options, res *Result) (truth, after []translate.Statement, err error) {
	d, err := importer.OpenSpecChange(opt.Repo, opt.Change)
	if err != nil {
		return nil, nil, err
	}
	truth = opt.Truth
	if truth == nil {
		if truth, err = importer.OpenSpec(opt.Repo, false); err != nil {
			return nil, nil, err
		}
	}
	after, res.Warnings = d.Apply(truth)
	res.Renamed = d.Renamed
	for _, s := range d.Added {
		res.Added = append(res.Added, s.ID)
	}
	for _, s := range d.Modified {
		res.Modified = append(res.Modified, s.ID)
	}
	res.Removed = d.Removed
	return truth, after, nil
}

// specRoot is where a format keeps its specs, relative to the repository.
func specRoot(format string) string {
	switch format {
	case "kiro":
		return filepath.Join(".kiro", "specs")
	case "openspec":
		return filepath.Join("openspec", "specs")
	case "shale":
		return ".shale"
	default:
		return "specs"
	}
}

func importFormat(format, repo string) ([]translate.Statement, error) {
	var stmts []translate.Statement
	var err error
	switch format {
	case "speckit":
		stmts, err = importer.SpecKit(repo)
	case "kiro":
		stmts, err = importer.Kiro(repo)
	case "openspec":
		stmts, err = importer.OpenSpec(repo, false)
	case "shale":
		stmts, err = importer.Shale(repo)
	default:
		return nil, fmt.Errorf("format %q: use speckit, kiro, openspec or shale", format)
	}
	if errors.Is(err, importer.ErrNoSpecs) {
		return nil, nil // no specs yet: every statement is new
	}
	return stmts, err
}

// branchChange compares the specs at the base ref with the working tree:
// what a Spec Kit or Kiro feature branch adds, edits and removes, or the
// requests its coding agents declared with Shale (an intent the owner
// retires is removed).
//
// When opt.Truth is given (the statements the registry was built from, which
// may be a subset), the change is still measured against the specs at the
// base ref, and the statements after it are the truth's plus what the
// change adds or edits.
func branchChange(opt Options, res *Result) (truth, after []translate.Statement, err error) {
	if err := baseHoldsRegistry(opt); err != nil {
		return nil, nil, err
	}
	if opt.Format == "shale" {
		return shaleChange(opt, res)
	}
	dir, err := os.MkdirTemp("", "stele-base-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	if err := extract(opt.Repo, opt.Base, specRoot(opt.Format), dir); err != nil {
		return nil, nil, err
	}
	atBase, err := importFormat(opt.Format, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("specs at %s: %w", opt.Base, err)
	}
	now, err := importFormat(opt.Format, opt.Repo)
	if err != nil {
		return nil, nil, err
	}
	before := map[string]string{}
	for _, s := range atBase {
		before[s.ID] = s.Prose
	}
	kept := map[string]bool{}
	for _, s := range now {
		kept[s.ID] = true
		switch prose, ok := before[s.ID]; {
		case !ok:
			res.Added = append(res.Added, s.ID)
		case prose != s.Prose:
			res.Modified = append(res.Modified, s.ID)
		}
	}
	truth = opt.Truth
	if truth == nil {
		truth = atBase
	}
	known := map[string]bool{}
	for _, s := range truth {
		known[s.ID] = true
	}
	for _, s := range atBase {
		if !kept[s.ID] && known[s.ID] {
			res.Removed = append(res.Removed, s.ID)
		}
	}
	changed := map[string]bool{}
	for _, id := range append(append([]string{}, res.Added...), res.Modified...) {
		changed[id] = true
	}
	for _, s := range now {
		if known[s.ID] || changed[s.ID] {
			after = append(after, s)
		}
	}
	return truth, after, nil
}

// shaleChange measures the requests declared with Shale against the
// approved registry, not the specs at the base ref: a request is new until
// the owner approves it, wherever its evidence is. Measured against the
// base, a request committed before it was checked was already "at the
// base" on the branch that is the base (main), so it was never asked
// about; and an approved request whose evidence went missing was no
// removal, so its rules blocked every commit with nothing to answer (a
// developer trial, 2026-11-03). Requests the owner retired, and approved
// ones that are gone, are removed with their rules.
func shaleChange(opt Options, res *Result) (truth, after []translate.Statement, err error) {
	now, err := importFormat(opt.Format, opt.Repo)
	if err != nil {
		return nil, nil, err
	}
	return registryChange(opt, res, now)
}

// markdownChange measures the plain-Markdown requirements under
// opt.SpecsDir against the approved registry, as shaleChange does and for
// the same reason: a project that keeps its requirements in a folder often
// commits straight to its main branch, and a requirement committed before
// it was checked is still new. It needs no base ref, so it works in a
// repository with no commit yet.
func markdownChange(opt Options, res *Result) (truth, after []translate.Statement, err error) {
	now, err := importer.Markdown(opt.SpecsDir, importer.MarkdownOptions{AnyCase: opt.AnyCase, Exclude: []string{opt.Registry}})
	// No folder or no file yet: no requirement, as a new project has.
	if err != nil && !errors.Is(err, importer.ErrNoSpecs) {
		return nil, nil, err
	}
	return registryChange(opt, res, now)
}

// registryChange is the change from the approved registry to the
// statements the project holds now.
func registryChange(opt Options, res *Result, now []translate.Statement) (truth, after []translate.Statement, err error) {
	reg, err := ir.LoadDir(opt.Registry)
	if err != nil {
		return nil, nil, fmt.Errorf("registry %s: %w", opt.Registry, err)
	}
	held := map[string]translate.Statement{}
	for _, x := range reg.Rules {
		if _, ok := held[x.StatementID]; ok || x.StatementID == "" || !x.Live() {
			continue
		}
		held[x.StatementID] = translate.Statement{ID: x.StatementID, Prose: x.Prose, Source: x.Source, Introduced: x.Introduced}
	}
	kept := map[string]bool{}
	for _, s := range now {
		kept[s.ID] = true
		switch _, ok := held[s.ID]; {
		case !ok:
			res.Added = append(res.Added, s.ID)
		case !approvedAlready(reg, map[string]string{s.ID: s.Prose}):
			res.Modified = append(res.Modified, s.ID)
		default:
			held[s.ID] = s // as approved: the registry need not keep the text
		}
	}
	for _, s := range held {
		truth = append(truth, s)
		if !kept[s.ID] {
			res.Removed = append(res.Removed, s.ID)
		}
	}
	slices.SortFunc(truth, func(a, b translate.Statement) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(res.Removed)
	if opt.Truth != nil {
		truth = opt.Truth
	}
	return truth, now, nil
}

// extract writes the files under path at ref into dir. A path the ref does
// not have leaves dir empty.
// baseHoldsRegistry refuses a base ref that lacks the approved registry the
// current branch has committed: the change would be measured against the
// wrong branch. A registry outside the repository, or not committed, is not
// checked.
func baseHoldsRegistry(opt Options) error {
	rel, err := relTo(opt.Repo, opt.Registry)
	if err != nil || opt.Registry == "" {
		return nil
	}
	has := func(ref string) bool {
		return exec.Command("git", "-C", opt.Repo, "cat-file", "-e", ref+":./"+rel).Run() == nil
	}
	if has("HEAD") && !has(opt.Base) {
		if c := registryStart(opt.Repo, opt.Base, has); c != "" {
			return fmt.Errorf("base %s has no approved registry at %s: it was added in %s on this branch. Compare with the branch that holds it, or with that commit until it reaches %s (--base %s)", opt.Base, rel, c, opt.Base, c)
		}
		return fmt.Errorf("base %s has no approved registry at %s; compare with the branch that holds it (--base)", opt.Base, rel)
	}
	return nil
}

// registryStart is the first commit after base that has the registry, as
// a short hash and subject, or "".
func registryStart(repo, base string, has func(string) bool) string {
	out, err := exec.Command("git", "-C", repo, "rev-list", "--reverse", base+"..HEAD").Output()
	if err != nil {
		return ""
	}
	for _, c := range strings.Fields(string(out)) {
		if has(c) {
			short, err := exec.Command("git", "-C", repo, "rev-parse", "--short", c).Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(short))
		}
	}
	return ""
}

func extract(repo, ref, path, dir string) error {
	var stderr bytes.Buffer
	cmd := exec.Command("git", "-C", repo, "archive", "--format=tar", ref, "--", filepath.ToSlash(path))
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if strings.Contains(stderr.String(), "did not match any files") {
			return nil
		}
		return fmt.Errorf("git archive %s: %v: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("git archive: unsafe path %q", h.Name)
		}
		p := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			src, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, src, 0o644); err != nil {
				return err
			}
		}
	}
}
