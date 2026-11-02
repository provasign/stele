package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/translate"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// doorProject is a git repository with one approved requirement, "The door
// MUST be locked", committed.
func doorProject(t *testing.T, withRules bool) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prose := "Section: Door\nREQ-1: The door MUST be locked."
	sum := sha256.Sum256([]byte(prose))
	hash := hex.EncodeToString(sum[:])[:16]
	id := "door.md#REQ-1"
	write("specs/door.md", "# Door\n\nREQ-1: The door MUST be locked.\n")
	stmts, _ := json.Marshal([]map[string]any{{"id": id, "source": "door.md#L3", "prose": prose, "reference": []string{}}})
	write("statements.yaml", string(stmts))
	write("registry/context.yaml", "context: c\nformatVersion: 1\n---\nentity: Door\ncontext: c\nattributes:\n  locked: {type: bool, controller: system}\n---\noperation: Door.close\ncontext: c\ndefault: allow\nwrites: [Door.locked]\n")
	if withRules {
		src, _ := json.Marshal(map[string]string{"statementId": id, "doc": "door.md#L3", "textHash": hash, "statementHash": hash, "prose": prose})
		write("registry/rules.yaml", "id: R1\nkind: invariant\nassert: door.locked\nsource: "+string(src)+"\n")
	}
	cfg, _ := json.Marshal(map[string]any{"formatVersion": 1, "registry": "registry", "translate": map[string]any{"llm": "claude", "drafts": 1}, "specs": map[string]any{"root": "specs", "format": "markdown", "snapshot": "statements.yaml"}})
	write("stele.yaml", string(cfg))
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"}, {"add", "-A"}, {"commit", "-qm", "start"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func precommit(t *testing.T, dir string, stage ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"add", "--"}, stage...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	var out bytes.Buffer
	code := cmdPrecommit(nil, &out)
	return code, out.String()
}

// TestPrecommit: silent for unrelated commits; blocks, with the next
// command, an approved requirement whose text changed and a staged rule
// that contradicts an approved one without approval.
func TestPrecommit(t *testing.T) {
	dir := doorProject(t, true)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	if code, out := precommit(t, dir, "notes.txt"); code != 0 || out != "" {
		t.Errorf("unrelated commit: %d %q", code, out)
	}

	os.WriteFile(filepath.Join(dir, "specs/door.md"), []byte("# Door\n\nREQ-1: The door MUST be locked at night.\n"), 0o644)
	code, out := precommit(t, dir, "specs/door.md")
	if code != 1 || !strings.Contains(out, "R1 was approved for text that has since changed") || !strings.Contains(out, "Next: run `stele watch --once --json`") || !strings.Contains(out, "--no-verify") {
		t.Errorf("changed requirement: %d\n%s", code, out)
	}
	exec.Command("git", "-C", dir, "reset", "-q").Run()
	exec.Command("git", "-C", dir, "checkout", "-q", "--", "specs/door.md").Run()

	rules, _ := os.ReadFile(filepath.Join(dir, "registry/rules.yaml"))
	os.WriteFile(filepath.Join(dir, "registry/rules.yaml"), append(rules, []byte("---\nid: R2\nkind: invariant\nassert: not door.locked\n")...), 0o644)
	code, out = precommit(t, dir, "registry/rules.yaml")
	if code != 1 || !strings.Contains(out, "R2 is not approved") {
		t.Errorf("unapproved contradicting rule: %d\n%s", code, out)
	}
}

// TestPrecommitNotAdopted: before a registry has rules, a spec commit passes
// with the command that adopts the specs.
func TestPrecommitNotAdopted(t *testing.T) {
	dir := doorProject(t, false)
	os.WriteFile(filepath.Join(dir, "specs/door.md"), []byte("# Door\n\nREQ-1: The door MUST be shut.\n"), 0o644)
	code, out := precommit(t, dir, "specs/door.md")
	if code != 0 || !strings.Contains(out, "not adopted yet") || !strings.Contains(out, "stele adopt --json") {
		t.Errorf("%d\n%s", code, out)
	}
}

// TestPrecommitNoRequirements: a Shale project before its first request
// has nothing to adopt, so committing its setup says nothing.
func TestPrecommitNoRequirements(t *testing.T) {
	dir := doorProject(t, false)
	cfg, _ := json.Marshal(map[string]any{"formatVersion": 1, "registry": "registry", "translate": map[string]any{"llm": "claude", "drafts": 1}, "specs": map[string]any{"root": ".", "format": "shale", "snapshot": "statements.yaml"}})
	os.WriteFile(filepath.Join(dir, "stele.yaml"), cfg, 0o644)
	os.MkdirAll(filepath.Join(dir, ".shale"), 0o755)
	os.WriteFile(filepath.Join(dir, ".shale/config.yaml"), []byte("version: 1\n"), 0o644)
	if code, out := precommit(t, dir, "stele.yaml", ".shale/config.yaml"); code != 0 || out != "" {
		t.Errorf("%d %q", code, out)
	}
}

// TestInstallGitHook: init writes the pre-commit hook once, and leaves a
// hook it did not write alone, saying the line to add.
func TestInstallGitHook(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	path, note, err := installGitHook(dir)
	if err != nil || note != "" || !strings.HasSuffix(path, filepath.Join("hooks", "pre-commit")) {
		t.Fatalf("first install: %q %q %v", path, note, err)
	}
	if b, _ := os.ReadFile(path); string(b) != gitHookScript {
		t.Fatalf("hook: %s", b)
	}
	if again, note, err := installGitHook(dir); again != "" || note != "" || err != nil {
		t.Errorf("second install changed something: %q %q %v", again, note, err)
	}
	os.WriteFile(path, []byte("#!/bin/sh\nmake lint\n"), 0o755)
	if p, note, err := installGitHook(dir); p != "" || err != nil || !strings.Contains(note, "stele precommit || exit 1") {
		t.Errorf("foreign hook: %q %q %v", p, note, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "#!/bin/sh\nmake lint\n" {
		t.Errorf("foreign hook was changed: %s", b)
	}
}

// TestNotAProject: outside a Stele project the hook passes silently.
func TestNotAProject(t *testing.T) {
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(t.TempDir())
	var out bytes.Buffer
	if code := cmdPrecommit(nil, &out); code != 0 || out.Len() != 0 {
		t.Errorf("%d %q", code, out.String())
	}
}

// TestPrecommitOpenSpecChange: in OpenSpec an approved change lives in
// openspec/changes/ until it is archived, so the registry already holds the
// change's reading while openspec/specs/ has the old text. The hook accepts
// that, and still blocks a rule whose text matches neither.
func TestPrecommitOpenSpecChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write("openspec/specs/doors/spec.md", "# Doors\n\n## Requirements\n\n### Requirement: Locked doors\nThe door MUST be locked.\n")
	var out bytes.Buffer
	if code := cmdInit([]string{dir, "--harness", "codex", "--git-hook=false"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("init: %s", out.String())
	}
	cfg, _ := config.Find(dir)
	newText := "Requirement: Locked doors\nThe door MUST be locked at night."
	rule := func(prose string) string {
		src, _ := json.Marshal(map[string]string{"statementId": "doors/Locked doors", "doc": "openspec/specs/doors/spec.md#L5", "textHash": translate.TextHash(prose), "prose": prose})
		return "context: main\nformatVersion: 1\n---\nentity: Door\ncontext: main\nattributes:\n  locked: {type: bool, controller: system}\n---\noperation: Door.close\ncontext: main\ndefault: allow\nwrites: [Door.locked]\n---\nid: R1\nkind: invariant\nassert: door.locked\nsource: " + string(src) + "\n"
	}
	write(".stele/registry/rules.yaml", rule("Requirement: Locked doors\nThe door MUST be locked."))
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"}, {"add", "-A"}, {"commit", "-qm", "start"}} {
		run(args...)
	}
	_ = cfg
	write("openspec/changes/night/specs/doors/spec.md", "# Doors\n\n## MODIFIED Requirements\n\n### Requirement: Locked doors\nThe door MUST be locked at night.\n")
	write(".stele/registry/rules.yaml", rule(newText))
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	run("add", "-A")
	var got bytes.Buffer
	if code := cmdPrecommit(nil, &got); code != 0 && strings.Contains(got.String(), "approved for text that has since changed") {
		t.Errorf("an approved, not yet archived change was blocked as stale:\n%s", got.String())
	}
	run("commit", "-qm", "approved change")
	write("openspec/changes/night/specs/doors/spec.md", "# Doors\n\n## MODIFIED Requirements\n\n### Requirement: Locked doors\nThe door MUST be locked at dawn.\n")
	run("add", "-A")
	got.Reset()
	if code := cmdPrecommit(nil, &got); code != 1 || !strings.Contains(got.String(), "R1 was approved for text that has since changed") {
		t.Errorf("a rule matching no text passed: %d\n%q", code, got.String())
	}
}

// TestSyncAfterArchive: once an approved OpenSpec change is archived into
// openspec/specs/, its requirements are not reported as needing
// translation, and `stele sync --apply` (paths from stele.yaml) moves the
// rules' provenance and the accepted statements to the specs as they are.
func TestSyncAfterArchive(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
	}
	write("openspec/specs/doors/spec.md", "# Doors\n\n## Requirements\n\n### Requirement: Locked doors\nThe door MUST be locked.\n")
	var out bytes.Buffer
	if code := cmdInit([]string{dir, "--harness", "codex", "--git-hook=false"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("init: %s", out.String())
	}
	cfg, _ := config.Find(dir)
	prose := "Requirement: Locked doors\nThe door MUST be locked at night."
	h := translate.TextHash(prose)
	src, _ := json.Marshal(map[string]string{"statementId": "doors/Locked doors", "doc": "openspec/changes/night/specs/doors/spec.md#L5", "textHash": h, "prose": prose, "approvedBy": "Dana", "approvedHash": h})
	write(".stele/registry/rules.yaml", "context: main\nformatVersion: 1\n---\nentity: Door\ncontext: main\nattributes:\n  locked: {type: bool, controller: system}\n---\nid: R1\nkind: invariant\nassert: door.locked\nsource: "+string(src)+"\n")
	// The archived change: its text is now the spec's.
	write("openspec/specs/doors/spec.md", "# Doors\n\n## Requirements\n\n### Requirement: Locked doors\nThe door MUST be locked at night.\n")
	r, err := evaluateSpecs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes.Added)+len(r.Changes.Changed) != 0 || len(r.StaleRules) != 0 {
		t.Errorf("an approved, archived requirement reported as new or changed: %+v stale %v", r.Changes, r.StaleRules)
	}
	if r.InSync {
		t.Fatal("provenance should still point at the change folder")
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	out.Reset()
	if code := cmdSync([]string{"--apply"}, &out); code != 0 {
		t.Fatalf("sync: %d %s", code, out.String())
	}
	if r, err = evaluateSpecs(cfg); err != nil || !r.InSync {
		t.Errorf("not in sync after `stele sync --apply`: %v %+v\n%s", err, r, out.String())
	}
}
