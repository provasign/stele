package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/translate"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/provasign/stele/internal/config"
)

// cmdInit creates stele.yaml and an empty registry, asking for each setting
// that has no flag unless --yes is given.
func cmdInit(args []string, in io.Reader, out io.Writer, interactive bool) int {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	llmFlag := fs.String("llm", "", "LLM backend for translation: anthropic, openai, claude, codex or cmd")
	drafts := fs.Int("drafts", 0, "drafts per requirement (1 is cheapest; above 1, disagreements become questions)")
	registry := fs.String("registry", "", "registry directory, relative to the repository root")
	yes := fs.Bool("yes", false, "accept defaults for anything not given as a flag; in a project already set up, refresh the agents recorded in stele.yaml")
	harnessFlag := fs.String("harness", "", "coding agents to set up to run Stele's loop, comma-separated: "+strings.Join(harnessIDs(), ", ")+" (default: ask, recommending the ones found)")
	interactiveFlag := fs.Bool("interactive", false, "ask about the model, drafts, registry and coding agents instead of using the defaults and the agents found")
	gitHook := fs.Bool("git-hook", true, "install the git pre-commit hook that blocks a commit whose specs and approved requirements disagree (--git-hook=false to skip)")
	hook := fs.Bool("hook", true, "with claude or codex: install the agent's hook that gives it the approved requirements a request touches, and tells it when saved specs drift (--hook=false to skip)")
	dir := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		// Set up: only the coding agents can be (re)installed.
		cfg, err := config.Find(dir)
		if err != nil {
			return fail(err)
		}
		// Set up: refresh the agents recorded (or found), never stele.yaml's
		// other settings.
		selected := cfg.Harnesses
		switch {
		case *harnessFlag != "":
			if selected, err = parseHarnesses(*harnessFlag); err != nil {
				return fail(err)
			}
		case len(selected) == 0:
			selected = detectHarnesses(dir)
		}
		return setUpAgents(dir, *cfg, selected, *hook, *gitHook, true, out)
	}

	c := config.Default()
	// A new project keeps Stele's files together, apart from any spec
	// format's own folders (specs/ is Spec Kit's).
	c.Registry = ".stele/registry"
	// One draft: `stele adopt` and `stele change` translate once and check
	// the reading themselves; drafts only set `stele translate`'s sampling.
	c.Translate.Drafts = 1
	c.Specs = detectSpecs(dir)
	ask := interactive && *interactiveFlag && !*yes
	r := bufio.NewReader(in)
	prompt := func(question, def string) (string, error) {
		fmt.Fprintf(out, "%s [%s]: ", question, def)
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		if line = strings.TrimSpace(line); line == "" {
			return def, nil
		}
		return line, nil
	}

	var err error
	switch {
	case *llmFlag != "":
		c.Translate.LLM = *llmFlag
	case !ask:
		var ok bool
		if c.Translate.LLM, ok = availableLLM(c.Translate.LLM); !ok {
			fmt.Fprintln(out, "No model found to translate specs with: Stele needs the claude or codex CLI on PATH, or ANTHROPIC_API_KEY or OPENAI_API_KEY. Install one and set translate.llm in stele.yaml (claude, codex, anthropic or openai).")
		}
	}
	switch {
	case *llmFlag != "":
	case ask:
		fmt.Fprintln(out, "Which LLM should translate spec prose into rules?")
		fmt.Fprintln(out, "  claude, codex     use the CLI you already have installed")
		fmt.Fprintln(out, "  anthropic, openai use an API key (ANTHROPIC_API_KEY or OPENAI_API_KEY)")
		if c.Translate.LLM, err = prompt("LLM", c.Translate.LLM); err != nil {
			return fail(err)
		}
	}
	switch {
	case *drafts != 0:
		c.Translate.Drafts = *drafts
	case ask:
		fmt.Fprintln(out, "\nHow many drafts per requirement?")
		fmt.Fprintln(out, "  1  cheapest: one translation per requirement, taken as is")
		fmt.Fprintln(out, "  3  each requirement is translated three times and the readings are compared;")
		fmt.Fprintln(out, "     agreed readings are used, disagreements become questions for you.")
		fmt.Fprintln(out, "     Costs three times as much. On the Project A case study it kept a bad draft")
		fmt.Fprintln(out, "     from adding false conflicts (4 instead of up to 11) and found 5 ambiguous")
		fmt.Fprintln(out, "     requirements in 14.")
		fmt.Fprintln(out, "  With Claude Opus 5.5, one draft cost about $0.004-0.01 per requirement.")
		ans, err := prompt("Drafts", strconv.Itoa(c.Translate.Drafts))
		if err != nil {
			return fail(err)
		}
		if c.Translate.Drafts, err = strconv.Atoi(ans); err != nil {
			return fail(fmt.Errorf("drafts must be a number, got %q", ans))
		}
	}
	switch {
	case *registry != "":
		c.Registry = *registry
	case ask:
		if c.Registry, err = prompt("\nRegistry directory", c.Registry); err != nil {
			return fail(err)
		}
	}
	selected := detectHarnesses(dir)
	switch {
	case *harnessFlag != "":
		if selected, err = parseHarnesses(*harnessFlag); err != nil {
			return fail(err)
		}
	case ask:
		found := detectHarnesses(dir)
		fmt.Fprintln(out, "\nWhich coding agents should run Stele's loop when they edit specs?")
		fmt.Fprintf(out, "  known: %s\n", strings.Join(harnessIDs(), ", "))
		if len(found) > 0 {
			fmt.Fprintf(out, "  found here: %s\n", harnessLabels(found))
		}
		ans, err := prompt("Agents (comma-separated, or none)", strings.Join(found, ","))
		if err != nil {
			return fail(err)
		}
		selected = nil
		if strings.TrimSpace(ans) != "none" {
			if selected, err = parseHarnesses(ans); err != nil {
				return fail(err)
			}
		}
	}
	if err := c.Validate(); err != nil {
		return fail(err)
	}

	reg := filepath.Join(dir, c.Registry)
	if err := os.MkdirAll(reg, 0o755); err != nil {
		return fail(err)
	}
	if err := writeSteleIgnore(dir); err != nil {
		return fail(err)
	}
	found := 0
	if c.Specs != nil {
		// The specs as they are now are the starting point: saved-spec sync
		// reports what changes from here.
		stmts, err := importSpecs(dir, c.Specs)
		// A new Spec Kit, OpenSpec or Kiro project has its folders and no
		// spec yet: each spec is checked as it is written. Turning sync off
		// left stele.yaml without the specs, so nothing named the format
		// (a developer trial, 2026-11-05).
		empty := errors.Is(err, importer.ErrNoSpecs)
		if empty {
			stmts, err = nil, nil
		}
		if err == nil {
			err = writeSnapshot(filepath.Join(dir, c.Specs.Snapshot), stmts, c.Specs.Format)
		}
		if err != nil {
			fmt.Fprintf(out, "Could not read the %s specs (%v); saved-spec sync stays off until stele.yaml names them.\n", c.Specs.Format, err)
			c.Specs = nil
		} else if empty {
			fmt.Fprintf(out, "Found a %s project with no specs yet; Stele checks each one as it is written.\n", formatLabel(c.Specs.Format))
		} else {
			fmt.Fprintf(out, "Found %s specs: %d requirement(s).\n", formatLabel(c.Specs.Format), len(stmts))
			found = len(stmts)
		}
	}
	ctxFile := filepath.Join(reg, "context.yaml")
	if _, err := os.Stat(ctxFile); errors.Is(err, os.ErrNotExist) {
		starter := "# The first context. Add entities, operations and rules beside it (docs/04).\ncontext: main\nformatVersion: 1\nowner: \"\"\n"
		if err := os.WriteFile(ctxFile, []byte(starter), 0o644); err != nil {
			return fail(err)
		}
	}
	if err := config.Write(dir, c); err != nil {
		return fail(err)
	}
	fmt.Fprintf(out, "Wrote %s (model: %s) and the registry %s.\n",
		config.FileName, llmLabel(c.Translate.LLM), c.Registry)
	code := setUpAgents(dir, c, selected, *hook, *gitHook, false, out)
	if code == 0 && found > 0 {
		fmt.Fprintf(out, "To start, ask your agent: \"adopt our specs with Stele\". It reads the %d requirement(s) once and asks you only what needs a person.\n", found)
	}
	return code
}

// setUpAgents installs Stele's loop for the selected agents and records them
// in stele.yaml. A refresh with the same selection changes nothing.
func setUpAgents(dir string, c config.Config, selected []string, hook, gitHook, existing bool, out io.Writer) int {
	changed, err := installSteering(dir, selected, hook)
	if err != nil {
		return fail(err)
	}
	if gitHook {
		path, note, err := installGitHook(dir)
		if err != nil {
			return fail(err)
		}
		if path != "" {
			changed = append(changed, path)
		}
		if note != "" {
			fmt.Fprintln(out, note)
		}
	}
	if !slices.Equal(c.Harnesses, selected) {
		c.Harnesses = selected
		if existing {
			if err := config.Write(dir, c); err != nil {
				return fail(err)
			}
		} else if err := config.Write(dir, c); err != nil {
			return fail(err)
		}
		changed = append(changed, filepath.Join(dir, config.FileName))
	}
	if len(selected) == 0 {
		fmt.Fprintf(out, "No coding agent found here; run `stele init --harness %s` to set one up.\n", strings.Join(harnessIDs(), ","))
	}
	if len(changed) == 0 {
		fmt.Fprintf(out, "Stele is already set up for %s.\n", harnessLabels(selected))
		return 0
	}
	if len(selected) > 0 {
		fmt.Fprintf(out, "Set up Stele for %s: %s\n", harnessLabels(selected), strings.Join(changed, ", "))
	} else {
		fmt.Fprintf(out, "Set up: %s\n", strings.Join(changed, ", "))
	}
	fmt.Fprintln(out, "Commit the files in the repository so everyone's agent works the same way. Now open your coding agent and work as usual: Stele gives it the approved requirements each request touches, and checks spec changes when you commit.")
	if hook && slices.Contains(selected, "codex") && slices.Contains(changed, filepath.Join(dir, ".codex", "hooks.json")) {
		// Codex runs a project's hooks only once the user has reviewed them;
		// that trust is the user's to give, so init never records it.
		fmt.Fprintln(out, "Codex runs a project's hooks only after you review them: in Codex, run /hooks and trust Stele's two hooks (once on each machine, and again when Stele updates them).")
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "stele init: %v\n", err)
	return 2
}

// stdinIsTerminal reports whether stdin is interactive.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// gitHookScript runs `stele precommit`; a clone without stele passes.
const gitHookScript = `#!/bin/sh
# Stele (stele init): block a commit whose specs and approved requirements
# disagree. A clone without stele on PATH is not checked; CI's gate is.
command -v stele >/dev/null 2>&1 || exit 0
exec stele precommit
`

// gitHookLine is what init adds to a pre-commit hook another tool wrote:
// the same check, which still blocks the commit when it fails.
const gitHookLine = "if command -v stele >/dev/null 2>&1; then stele precommit || exit 1; fi # stele (stele init)"

// installGitHook writes the pre-commit hook into the repository's hooks
// directory (core.hooksPath when set). It never replaces a hook it did not
// write: it returns a note saying the line to add instead. Outside a git
// repository it does nothing.
func installGitHook(dir string) (path, note string, err error) {
	cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = dir
	b, gerr := cmd.Output()
	if gerr != nil {
		return "", "", nil // not a git repository
	}
	hooks := strings.TrimSpace(string(b))
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(dir, hooks)
	}
	path = filepath.Join(hooks, "pre-commit")
	if old, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(old), "stele precommit") {
			return "", "", nil // Stele's, or a hook that runs it already: never rewritten
		}
		// Another tool's hook (Shale's, a linter's): Stele's line goes right
		// after the shebang, so it runs before anything there that could end
		// the script, and the hook keeps doing what it did.
		lines := strings.SplitAfter(string(old), "\n")
		at := 0
		if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
			at = 1
		}
		head := strings.Join(lines[:at], "")
		if at == 1 && !strings.HasSuffix(head, "\n") {
			head += "\n"
		}
		src := head + gitHookLine + "\n" + strings.Join(lines[at:], "")
		return path, fmt.Sprintf("added Stele's check to the existing pre-commit hook %s", path), os.WriteFile(path, []byte(src), 0o755)
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return "", "", err
	}
	return path, "", os.WriteFile(path, []byte(gitHookScript), 0o755)
}

// availableLLM is the translation backend this machine can run: the
// claude CLI, else the codex CLI, else an API key's provider, else def. A
// machine with Codex alone was set up for the claude CLI, and every
// translation failed (a developer trial, 2026-11-05). It reports whether
// any was found.
func availableLLM(def string) (string, bool) {
	for _, cli := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(cli); err == nil {
			return cli, true
		}
	}
	switch {
	case os.Getenv("ANTHROPIC_API_KEY") != "":
		return "anthropic", true
	case os.Getenv("OPENAI_API_KEY") != "":
		return "openai", true
	}
	return def, false
}

// detectSpecs finds the project's spec layout from its folders, or nil.
func detectSpecs(dir string) *config.Specs {
	is := func(p string) bool {
		fi, err := os.Stat(filepath.Join(dir, p))
		return err == nil && fi.IsDir()
	}
	snapshot := ".stele/accepted-statements.yaml"
	switch {
	case is("openspec"):
		return &config.Specs{Root: ".", Format: "openspec", Snapshot: snapshot}
	case is(".kiro/specs"):
		return &config.Specs{Root: ".", Format: "kiro", Snapshot: snapshot}
	case is(".specify"):
		return &config.Specs{Root: ".", Format: "speckit", Snapshot: snapshot}
	}
	// Spec Kit's feature folders (specs/001-name/spec.md), in a repository
	// that does not commit .specify/.
	if m, _ := filepath.Glob(filepath.Join(dir, "specs", "[0-9][0-9][0-9]-*", "spec.md")); len(m) > 0 {
		return &config.Specs{Root: ".", Format: "speckit", Snapshot: snapshot}
	}
	for _, d := range []string{"specs", "docs/specs"} {
		if is(d) {
			return &config.Specs{Root: d, Format: "markdown", Snapshot: snapshot}
		}
	}
	// No spec folder, but the agents declare each request with Shale: the
	// requests are the specs.
	if is(".shale") {
		return &config.Specs{Root: ".", Format: "shale", Snapshot: snapshot}
	}
	if is("docs") {
		return &config.Specs{Root: "docs", Format: "markdown", Snapshot: snapshot}
	}
	return nil
}

func formatLabel(format string) string {
	switch format {
	case "openspec":
		return "OpenSpec"
	case "speckit":
		return "Spec Kit"
	case "kiro":
		return "Kiro"
	case "shale":
		return "Shale intent"
	}
	return "Markdown"
}

// importSpecs reads the project's statements in its format.
func importSpecs(dir string, sp *config.Specs) ([]translate.Statement, error) {
	root := filepath.Join(dir, sp.Root)
	switch sp.Format {
	case "openspec":
		return importer.OpenSpec(root, false)
	case "kiro":
		return importer.Kiro(root)
	case "speckit":
		return importer.SpecKit(root)
	case "shale":
		return importer.Shale(root)
	}
	return importer.Markdown(root, importer.MarkdownOptions{AnyCase: sp.AnyCase})
}

// writeSnapshot writes the accepted statements as `stele import` does.
func writeSnapshot(path string, stmts []translate.Statement, format string) error {
	if stmts == nil {
		stmts = []translate.Statement{}
	}
	src, err := yaml.Marshal(stmts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	header := fmt.Sprintf("# %d accepted statements (%s): what saved-spec sync compares the specs with.\n", len(stmts), format)
	return os.WriteFile(path, append([]byte(header), src...), 0o644)
}

// writeSteleIgnore keeps Stele's working folders out of commits; the
// registry and the accepted statements beside them are committed.
func writeSteleIgnore(dir string) error {
	path := filepath.Join(dir, ".stele", ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("# Stele's working folders: not committed.\nchange/\naudit/\nadopt/\ncache/\n"), 0o644)
}

// llmLabel names a translation backend for people.
func llmLabel(backend string) string {
	switch backend {
	case "claude":
		return "your Claude Code CLI"
	case "codex":
		return "your Codex CLI"
	case "anthropic":
		return "the Anthropic API"
	case "openai":
		return "the OpenAI API"
	}
	return "a custom command"
}
