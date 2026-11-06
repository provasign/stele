package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/config"
)

func TestInitAsksForDrafts(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	// Answers: default LLM, 1 draft, default registry, no coding agent.
	if code := cmdInit([]string{dir, "--interactive"}, strings.NewReader("\n1\n\nnone\n"), &out, true); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "How many drafts per requirement?") {
		t.Errorf("the drafts question was not asked:\n%s", out.String())
	}
	c, err := config.Find(dir)
	if err != nil || c == nil {
		t.Fatalf("stele.yaml not readable: %v", err)
	}
	if c.Translate.Drafts != 1 || c.Translate.LLM != "agent" || c.Registry != ".stele/registry" {
		t.Errorf("config = %+v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, ".stele/registry/context.yaml")); err != nil {
		t.Errorf("no starter context: %v", err)
	}
	if code := cmdInit([]string{dir, "--yes", "--drafts", "5"}, strings.NewReader(""), &out, true); code != 0 {
		t.Errorf("a second init should refresh the set-up, exit %d: %s", code, out.String())
	}
	if c, _ := config.Find(dir); c == nil || c.Translate.Drafts != 1 {
		t.Errorf("a second init changed stele.yaml: %+v", c)
	}
}

func TestInitNonInteractiveUsesFlagsAndDefaults(t *testing.T) {
	// No model CLI or key on this PATH: the default stays, with a note.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	dir := t.TempDir()
	var out bytes.Buffer
	if code := cmdInit([]string{dir, "--drafts", "5"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "?") {
		t.Errorf("asked a question without a terminal:\n%s", out.String())
	}
	c, _ := config.Find(dir)
	if c.Translate.Drafts != 5 || c.Translate.LLM != "agent" {
		t.Errorf("config = %+v", c)
	}
	if code := cmdInit([]string{t.TempDir(), "--drafts", "0", "--yes"}, strings.NewReader(""), &out, false); code != 0 {
		t.Errorf("--drafts 0 means not given; want defaults, got exit %d", code)
	}
}

// init sets up every selected agent's instruction file and skills, keeps
// the rest of each file, gives a shared or symlinked file one section, is
// idempotent, and removes only what a deselected agent no longer needs.
func TestInitSetsUpAgents(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Project notes\n\nUse tabs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	// A project hook of the user's own stays beside Stele's.
	theirs := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "their-tool capture"}]}]}}`
	if err := os.MkdirAll(filepath.Join(dir, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex", "hooks.json"), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	all := "claude,codex,cursor,windsurf,vscode,gemini,opencode"
	if code := cmdInit([]string{dir, "--yes", "--harness", all, "--hook"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	read := func(p string) string {
		b, _ := os.ReadFile(filepath.Join(dir, p))
		return string(b)
	}
	agents := read("AGENTS.md")
	if !strings.HasPrefix(agents, "# Project notes\n\nUse tabs.\n") || strings.Count(agents, steeringBegin) != 1 || !strings.Contains(agents, "stele change openspec") {
		t.Fatalf("AGENTS.md (shared by codex, cursor, windsurf, opencode, and CLAUDE.md's target):\n%s", agents)
	}
	for _, p := range []string{"GEMINI.md", ".github/copilot-instructions.md"} {
		if strings.Count(read(p), steeringBegin) != 1 {
			t.Fatalf("%s lacks Stele's section", p)
		}
	}
	for _, p := range []string{".claude/skills/stele-change/SKILL.md", ".agents/skills/stele-change/SKILL.md", ".claude/hooks/stele-hook.js", ".codex/hooks/stele-hook.js"} {
		if read(p) == "" {
			t.Fatalf("%s not written", p)
		}
	}
	if !strings.Contains(read(".claude/settings.json"), "stele-hook.js") {
		t.Fatalf("hook not in settings: %s", read(".claude/settings.json"))
	}
	codex := read(".codex/hooks.json")
	if strings.Count(codex, ".codex/hooks/stele-hook.js") != 2 || !strings.Contains(codex, "UserPromptSubmit") || !strings.Contains(codex, "their-tool capture") {
		t.Fatalf("Codex hooks: %s", codex)
	}
	if !strings.Contains(codex, "2>/dev/null") {
		t.Fatalf("the Codex hook command is escaped, not as written: %s", codex)
	}
	if !strings.Contains(out.String(), "/hooks") {
		t.Fatalf("init does not say Codex asks to review its hooks:\n%s", out.String())
	}
	c, err := config.Find(dir)
	if err != nil || strings.Join(c.Harnesses, ",") != all {
		t.Fatalf("harnesses recorded = %v, %v", c, err)
	}
	// Again: nothing changes.
	out.Reset()
	if code := cmdInit([]string{dir, "--yes", "--hook"}, strings.NewReader(""), &out, false); code != 0 || !strings.Contains(out.String(), "already set up") {
		t.Fatalf("refresh: %d %s", code, out.String())
	}
	if strings.Count(read(".claude/settings.json"), "stele-hook.js") != 4 || !strings.Contains(read(".claude/settings.json"), "UserPromptSubmit") {
		t.Fatal("the hook was added twice, or not for each event")
	}
	if read(".codex/hooks.json") != codex || strings.Contains(out.String(), "/hooks") {
		t.Fatalf("refresh changed Codex hooks or repeated the review note:\n%s", out.String())
	}
	// Only Gemini now: AGENTS.md keeps the project's notes and loses Stele's
	// section; GEMINI.md keeps it.
	if code := cmdInit([]string{dir, "--harness", "gemini"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("narrow: %d %s", code, out.String())
	}
	if agents = read("AGENTS.md"); strings.Contains(agents, steeringBegin) || !strings.Contains(agents, "Use tabs.") {
		t.Fatalf("AGENTS.md after deselecting its agents:\n%s", agents)
	}
	if read(".claude/skills/stele-change/SKILL.md") != "" || read(".agents/skills/stele-change/SKILL.md") != "" {
		t.Fatal("a deselected agent's unedited skill copy should go")
	}
	if !strings.Contains(read("GEMINI.md"), steeringBegin) || read(".github/copilot-instructions.md") != "" {
		t.Fatal("GEMINI.md should keep Stele's section and Copilot's file (Stele's only) should be gone")
	}
}

// TestInitFindsOpenSpec: init records an OpenSpec project's layout and the
// specs as they are, so saved-spec sync reports the project in sync, and
// keeps Stele's working folders out of commits.
func TestInitFindsOpenSpec(t *testing.T) {
	dir := t.TempDir()
	spec := filepath.Join(dir, "openspec", "specs", "orders", "spec.md")
	os.MkdirAll(filepath.Dir(spec), 0o755)
	os.WriteFile(spec, []byte("# Orders\n\n## Requirements\n\n### Requirement: No cancel after shipping\nAn order MUST NOT be cancelled once it has shipped.\n\n#### Scenario: Shipped\n- **WHEN** a shipped order is cancelled\n- **THEN** the system refuses\n"), 0o644)
	var out bytes.Buffer
	if code := cmdInit([]string{dir, "--harness", "codex", "--git-hook=false"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "Found OpenSpec specs: 1 requirement(s).") {
		t.Errorf("init output:\n%s", out.String())
	}
	c, err := config.Find(dir)
	if err != nil || c.Specs == nil || c.Specs.Format != "openspec" || c.Registry != ".stele/registry" {
		t.Fatalf("config = %+v %v", c, err)
	}
	var w bytes.Buffer
	if code := cmdWatch([]string{"--project", dir, "--once", "--check"}, &w); code != 0 {
		t.Errorf("saved-spec sync right after init: exit %d\n%s", code, w.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".stele", ".gitignore")); !strings.Contains(string(b), "change/") {
		t.Errorf(".stele/.gitignore: %q", b)
	}
}

// detectSpecs reads Spec Kit's feature folders as Spec Kit even without
// .specify/, and other specs/ folders as plain Markdown.
// A new Spec Kit project, with .specify/ and no spec yet, keeps its specs
// in stele.yaml; and with only the codex CLI installed, Codex translates.
// Set up for the claude CLI, a Codex-only machine failed every translation
// (a developer trial, 2026-11-05).
func TestInitNewSpecKitProjectOnCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI is a shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".specify"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := cmdInit([]string{dir, "--yes", "--harness", "codex", "--hook=false", "--git-hook=false"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	c, err := config.Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Translate.LLM != "agent" || c.Specs == nil || c.Specs.Format != "speckit" {
		t.Fatalf("llm %q specs %+v\n%s", c.Translate.LLM, c.Specs, out.String())
	}
	if !strings.Contains(out.String(), "no specs yet") {
		t.Errorf("output: %s", out.String())
	}
	if strings.Contains(out.String(), "Node.js") {
		t.Errorf("warned about Node.js with no hook installed: %s", out.String())
	}
}

// TestInitWarnsWithoutNode: the agent hooks run with node; a machine
// without it got hooks that fail, and nothing said why (a developer trial,
// 2026-11-05).
func TestInitWarnsWithoutNode(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	var out bytes.Buffer
	if code := cmdInit([]string{dir, "--yes", "--llm", "codex", "--harness", "claude", "--git-hook=false"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "install Node.js") {
		t.Errorf("no Node.js warning: %s", out.String())
	}
}

func TestDetectSpecKitFeatureFolders(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"specs/001-loans/spec.md", "speckit"},
		{"specs/loans.md", "markdown"},
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, c.file)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("- **FR-001**: A member MUST NOT borrow.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectSpecs(dir); got == nil || got.Format != c.want {
			t.Errorf("%s: %+v, want %s", c.file, got, c.want)
		}
	}
}
