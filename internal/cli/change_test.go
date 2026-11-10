package cli

import (
	"bytes"
	"encoding/json"
	"github.com/provasign/stele/internal/change"
	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestChangeCommandLoop runs the OpenSpec change loop end to end with a
// command backend that replays fixed translations: a conflicting change is
// blocked, the fixed change applies, approval clears the gate.
func TestChangeCommandLoop(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("repo/openspec/specs/res/spec.md", "# Resources\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds.\n")
	write("repo/openspec/changes/erase/specs/res/spec.md", "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n")
	write("reg/vocabulary.yaml", "context: res\nformatVersion: 1\n---\nextend: platform.Request\ncontext: res\nattributes:\n  kind: { type: enum, values: [normal, erasure] }\n---\nentity: Resource\ncontext: res\nattributes:\n  holds: { type: int, range: [0, 5] }\n---\noperation: Resource.delete\ncontext: res\ndefault: allow\nwrites: []\n")
	write("reg/rules.yaml", "id: res/Delete guard\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: res/Delete guard\n  doc: openspec/specs/res/spec.md#L3\n")
	// The translator: a compact reply for the one statement each request holds.
	// The coverage check: each statement it asks about is one covered clause.
	covered := `{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}`
	rule := func(effect, when string) string {
		return `{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"permission","operation":"Resource.delete","effect":"` + effect + `","when":"` + when + `","overrides":[]}]}`
	}
	write("reply.sh", "in=$(cat)\ncase \"$in\" in\n"+
		"*'list its clauses'*) echo '"+covered+"' ;;\n"+
		"*'unless the request is an erasure'*) echo '"+rule("deny", "self.holds > 0 and request.kind != erasure")+"' ;;\n"+
		"*'Erasure requests MUST'*) echo '"+rule("allow", "request.kind = erasure")+"' ;;\n"+
		"*) echo '{}' ;;\nesac\n")
	llm := []string{"--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}
	run := func(want int, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		code := 0
		switch args[0] {
		case "change":
			code = cmdChange(append(args[1:], llm...), &out)
		case "approve":
			code = cmdApprove(args[1:], &out)
		case "gate":
			code = cmdGate(args[1:], &out)
		}
		if code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, out.String())
		}
		return out.String()
	}
	repo, reg, work := filepath.Join(dir, "repo"), filepath.Join(dir, "reg"), filepath.Join(dir, "work")
	run(0, "approve", "--registry", reg, "--all", "--by", "owner")
	if err := exec.Command("cp", "-r", reg, filepath.Join(dir, "main")).Run(); err != nil {
		t.Fatal(err)
	}
	out := run(1, "change", "openspec", repo, "erase", "--registry", reg, "--out", work)
	for _, want := range []string{"These requirements cannot all hold", "res › Requirement: Delete guard (current spec", "res › Requirement: Erasure (ADDED in this change", "rule `res/Erasure`: permission allow"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	write("repo/openspec/changes/erase/specs/res/spec.md", "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n\n## MODIFIED Requirements\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds, unless the request is an erasure.\n")
	out = run(0, "change", "openspec", repo, "erase", "--registry", reg, "--out", work, "--apply")
	if !strings.Contains(out, "no conflict with the approved rules") || !strings.Contains(out, "applied to") {
		t.Fatalf("fixed change:\n%s", out)
	}
	main := filepath.Join(dir, "main")
	if out := run(1, "gate", "--base", main, "--head", reg); !strings.Contains(out, "[UNAPPROVED] res/Erasure") {
		t.Fatalf("gate before approval:\n%s", out)
	}
	run(0, "approve", "--registry", reg, "--base", main, "--by", "owner")
	run(0, "gate", "--base", main, "--head", reg)
}

// TestChangeAnswerLoop runs the agent's loop (D-058): the check asks about
// the conflict, the change is fixed, and approving applies the change and
// clears the gate.
func TestChangeAnswerLoop(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("repo/openspec/specs/res/spec.md", "# Resources\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds.\n")
	write("repo/openspec/changes/erase/specs/res/spec.md", "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n")
	write("reg/vocabulary.yaml", "context: res\nformatVersion: 1\n---\nextend: platform.Request\ncontext: res\nattributes:\n  kind: { type: enum, values: [normal, erasure] }\n---\nentity: Resource\ncontext: res\nattributes:\n  holds: { type: int, range: [0, 5] }\n---\noperation: Resource.delete\ncontext: res\ndefault: allow\nwrites: []\n")
	write("reg/rules.yaml", "id: res/Delete guard\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: res/Delete guard\n  doc: openspec/specs/res/spec.md#L3\n")
	// The coverage check: each statement it asks about is one covered clause.
	covered := `{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}`
	rule := func(effect, when string) string {
		return `{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"permission","operation":"Resource.delete","effect":"` + effect + `","when":"` + when + `","overrides":[]}]}`
	}
	write("reply.sh", "in=$(cat)\ncase \"$in\" in\n"+
		"*'list its clauses'*) echo '"+covered+"' ;;\n"+
		"*'unless the request is an erasure'*) echo '"+rule("deny", "self.holds > 0 and request.kind != erasure")+"' ;;\n"+
		"*'Erasure requests MUST'*) echo '"+rule("allow", "request.kind = erasure")+"' ;;\n"+
		"*) echo '{}' ;;\nesac\n")
	repo, reg, work, main := filepath.Join(dir, "repo"), filepath.Join(dir, "reg"), filepath.Join(dir, "work"), filepath.Join(dir, "main")
	var out bytes.Buffer
	if cmdApprove([]string{"--registry", reg, "--all", "--by", "owner"}, &out) != 0 || exec.Command("cp", "-r", reg, main).Run() != nil {
		t.Fatal(out.String())
	}
	check := func(want int) changeSummary {
		t.Helper()
		var out bytes.Buffer
		if code := cmdChange([]string{"openspec", repo, "erase", "--registry", reg, "--out", work, "--json", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}, &out); code != want {
			t.Fatalf("change: exit %d, want %d\n%s", code, want, out.String())
		}
		var s changeSummary
		if err := json.Unmarshal(out.Bytes(), &s); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		return s
	}
	answer := func(want int, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if code := cmdChange(append([]string{"answer", "erase"}, append(args, "--out", work, "--by", "owner")...), &out); code != want {
			t.Fatalf("answer %v: exit %d, want %d\n%s", args, code, want, out.String())
		}
		return out.String()
	}
	s := check(1)
	if s.Status != "questions" || len(s.Questions) != 1 || s.Questions[0].Kind != "conflict" {
		t.Fatalf("first check: %+v", s)
	}
	q := s.Questions[0]
	answer(2, q.ID, "misread") // needs the owner's words
	if out := answer(0, q.ID, "update_old"); !strings.Contains(out, "MODIFIED") {
		t.Fatalf("answer: %s", out)
	}
	// The agent updates the older requirement in the change, as answered.
	write("repo/openspec/changes/erase/specs/res/spec.md", "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n\n## MODIFIED Requirements\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds, unless the request is an erasure.\n")
	s = check(0)
	if s.Status != "ready" || s.Reused != 1 || s.Questions[0].Kind != "approve" {
		t.Fatalf("second check: %+v", s)
	}
	if out := answer(0, s.Questions[0].ID, "approve"); !strings.Contains(out, "approved 2 rule(s)") {
		t.Fatalf("approve: %s", out)
	}
	out.Reset()
	if code := cmdGate([]string{"--base", main, "--head", reg}, &out); code != 0 {
		t.Fatalf("gate after approval: %d\n%s", code, out.String())
	}
}

// TestBranchChangeName: a change takes its branch's name until the owner
// approved a change of that name; the next change on the branch takes the
// next free one, so it never writes over the approved change's files.
func TestBranchChangeName(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo, registry := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "feature/orders"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if got := branchChangeName(repo, registry); got != "feature-orders" {
		t.Fatalf("first change = %q", got)
	}
	os.WriteFile(filepath.Join(registry, "change-feature-orders.yaml"), nil, 0o644)
	if got := branchChangeName(repo, registry); got != "feature-orders-2" {
		t.Fatalf("after an approved change = %q", got)
	}
	os.WriteFile(filepath.Join(registry, "change-feature-orders-2.vocabulary.yaml"), nil, 0o644)
	if got := branchChangeName(repo, registry); got != "feature-orders-3" {
		t.Fatalf("after two = %q", got)
	}
}

// A change whose translation failed is reported as failed, never clean, and
// `--help` after a format prints the help instead of asking for --base.
func TestChangeFailedStatusAndHelp(t *testing.T) {
	res := &change.Result{Change: "c1", Gate: &gate.Result{Accepted: true}, Failed: map[string]string{"intent/a": "backend: Not logged in"}}
	if s := summarize(res, t.TempDir()); s.Status != "failed" || len(s.Failed) != 1 || !strings.Contains(s.Next, "run the same check again") {
		t.Fatalf("summary = %+v", s)
	}
	var out bytes.Buffer
	if code := cmdChange([]string{"shale", "--help"}, &out); code != 0 || !strings.Contains(out.String(), "--base REF") || !strings.Contains(out.String(), "-max-cost") {
		t.Fatalf("help: %d\n%s", code, out.String())
	}
}

// A model that hangs is cut off per call and reported as failed, and a run
// that hits its overall deadline still answers in JSON: the agent reads the
// JSON, and a check that printed none left it stuck (2026-11-02).
func TestChangeHangingModelAnswersInJSON(t *testing.T) {
	dir := doorProject(t, false)
	// The door project with the requests declared in Shale as its specs.
	cfg, _ := json.Marshal(map[string]any{"formatVersion": 1, "registry": "registry", "translate": map[string]any{"llm": "claude", "drafts": 1}, "specs": map[string]any{"root": ".", "format": "shale", "snapshot": "statements.yaml"}})
	os.WriteFile(filepath.Join(dir, "stele.yaml"), cfg, 0o644)
	os.MkdirAll(filepath.Join(dir, ".shale", "local"), 0o755)
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "shale"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, ".shale", "local", "s1.jsonl"), []byte(`{"kind":"intent","at":"2026-11-02T10:00:00Z","title":"Close the door at night"}`+"\n"), 0o644)
	if code := cmdKeep([]string{"--project", dir}, io.Discard); code != 0 {
		t.Fatalf("keep: %d", code)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)
	for _, limit := range []string{"--request-timeout=200ms", "--timeout=500ms"} {
		var out bytes.Buffer
		code := cmdChange([]string{"shale", ".", "--base", "HEAD", "--json", "--llm", "cmd", "--llm-cmd", "sleep 30", "--out", t.TempDir(), limit}, &out)
		var s changeSummary
		if err := json.Unmarshal(out.Bytes(), &s); err != nil || code != 1 || s.Status != "failed" {
			t.Fatalf("%s: exit %d, %v\n%s", limit, code, err, out.String())
		}
	}
}

// Answering a request is a one-off task sets it aside in Stele's own file,
// with why: Shale's evidence is never edited.
func TestAnswerTaskSetsRequestAside(t *testing.T) {
	repo, out := t.TempDir(), t.TempDir()
	q := change.Question{ID: "task-1", Kind: "task", AskOwner: true, Statements: []string{"intent/s1"},
		Options: []change.Option{{Choice: "task", Label: "A one-off task, not a requirement"}, {Choice: "requirement", Label: "A requirement"}}}
	js, _ := json.Marshal(change.Result{Change: "c1", Format: "shale", Repo: repo, Questions: []change.Question{q}, Gate: &gate.Result{}})
	os.WriteFile(filepath.Join(out, "result.json"), js, 0o644)
	var buf bytes.Buffer
	if code := cmdChangeAnswer([]string{"c1", "task-1", "task", "--out", out, "--by", "owner"}, &buf); code != 0 {
		t.Fatalf("exit %d: %s", code, buf.String())
	}
	src, _ := os.ReadFile(filepath.Join(repo, ".stele", "retired-intents.yaml"))
	if !strings.Contains(string(src), "id: intent/s1") || !strings.Contains(string(src), "why: not a requirement") {
		t.Fatalf("retired-intents.yaml:\n%s", src)
	}
}

// TestCopyDirMirrors: applying the checked head leaves the registry exactly
// as checked; a file the check dropped is removed, not kept (a code
// review, 2026-11-03).
func TestCopyDirMirrors(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(from, "lessons"), 0o755)
	os.WriteFile(filepath.Join(from, "rules.yaml"), []byte("new"), 0o644)
	os.WriteFile(filepath.Join(from, "lessons", "l1.yaml"), []byte("l"), 0o644)
	os.WriteFile(filepath.Join(to, "rules.yaml"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(to, "change-c1.writes.yaml"), []byte("stale"), 0o644)
	if err := copyDir(from, to); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(to, "rules.yaml")); string(b) != "new" {
		t.Errorf("rules.yaml = %q", b)
	}
	if _, err := os.Stat(filepath.Join(to, "lessons", "l1.yaml")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(to, "change-c1.writes.yaml")); !os.IsNotExist(err) {
		t.Errorf("a file the head lacks was kept: %v", err)
	}
}

// TestRemovalOnlyChangeIsApproved: a change that only removes a requirement
// comes to the approval, and approving retires the rule in the registry; it
// used to report clean with the rule still live (a code review,
// 2026-11-03).
func TestRemovalOnlyChangeIsApproved(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("repo/openspec/specs/res/spec.md", "# Resources\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds.\n")
	write("repo/openspec/changes/drop/specs/res/spec.md", "## REMOVED Requirements\n\n### Requirement: Delete guard\n**Reason**: holds are gone.\n")
	write("reg/vocabulary.yaml", "context: res\nformatVersion: 1\n---\nentity: Resource\ncontext: res\nattributes:\n  holds: { type: int, range: [0, 5] }\n---\noperation: Resource.delete\ncontext: res\ndefault: allow\nwrites: []\n")
	write("reg/rules.yaml", "id: res/Delete guard\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: res/Delete guard\n  doc: openspec/specs/res/spec.md#L3\n")
	repo, reg, work := filepath.Join(dir, "repo"), filepath.Join(dir, "reg"), filepath.Join(dir, "work")
	var out bytes.Buffer
	if cmdApprove([]string{"--registry", reg, "--all", "--by", "owner"}, &out) != 0 {
		t.Fatal(out.String())
	}
	out.Reset()
	if code := cmdChange([]string{"openspec", repo, "drop", "--registry", reg, "--out", work, "--json", "--llm-cmd", "echo {}"}, &out); code != 0 {
		t.Fatalf("change: exit %d\n%s", code, out.String())
	}
	var s changeSummary
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" {
		t.Fatalf("status %s, questions %+v", s.Status, s.Questions)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", "drop", s.Questions[0].ID, "approve", "--out", work, "--by", "owner"}, &out); code != 0 {
		t.Fatalf("approve: exit %d\n%s", code, out.String())
	}
	r, err := ir.LoadDir(reg)
	if err != nil {
		t.Fatal(err)
	}
	if x := r.Rule("res/Delete guard"); x == nil || x.Live() {
		t.Errorf("the removed requirement's rule is still live: %+v", x)
	}
}

// A repository with no specs, no commit and no spec tool: init gives it a
// requirements folder, and one command checks its first requirement, its
// later ones, and nothing at all. The path stopped at "lstat requirements:
// no such file or directory", and then at a base branch that did not exist
// (a review, 2026-11-09).
func TestFreshProjectMarkdownLoop(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	t.Chdir(dir)
	t.Setenv("STELE_APPROVAL_PROMPT", "")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(rel, src string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rel, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := cmdInit([]string{".", "--yes", "--harness", "claude", "--hook"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("init: %d\n%s", code, out.String())
	}
	cfg, err := config.Find(".")
	if err != nil || cfg.Specs == nil || cfg.Specs.Format != "markdown" || cfg.Specs.Root != "requirements" {
		t.Fatalf("stele.yaml = %+v, %v", cfg, err)
	}
	if !strings.Contains(out.String(), "requirements/") || strings.Contains(out.String(), "adopt our specs") {
		t.Fatalf("init does not say where requirements go, or offers an adoption with nothing to adopt:\n%s", out.String())
	}
	if c := nextCommand(cfg, dir); c != "stele change markdown . --json" {
		t.Fatalf("next command = %q", c)
	}
	covered := `{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}`
	rule := func(effect, when string) string {
		return `{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"permission","operation":"Resource.delete","effect":"` + effect + `","when":"` + when + `","overrides":[]}]}`
	}
	write("reply.sh", "in=$(cat)\ncase \"$in\" in\n"+
		"*'list its clauses'*) echo '"+covered+"' ;;\n"+
		"*'Erasure requests MUST'*) echo '"+rule("allow", "request.kind = erasure")+"' ;;\n"+
		"*'MUST NOT delete a resource with holds'*) echo '"+rule("deny", "self.holds > 0")+"' ;;\n"+
		"*) echo '{}' ;;\nesac\n")
	check := func(want int) changeSummary {
		t.Helper()
		var out bytes.Buffer
		if code := cmdChange([]string{"markdown", ".", "--json", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}, &out); code != want {
			t.Fatalf("change: exit %d, want %d\n%s", code, want, out.String())
		}
		var s changeSummary
		if err := json.Unmarshal(out.Bytes(), &s); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		return s
	}
	// Before the first requirement: nothing to check, and no error.
	if s := check(0); s.Status != "clean" {
		t.Fatalf("empty project: %+v", s)
	}
	write(filepath.Join(cfg.RegistryDir(), "context.yaml"), "context: res\nformatVersion: 1\n---\nextend: platform.Request\ncontext: res\nattributes:\n  kind: { type: enum, values: [normal, erasure] }\n---\nentity: Resource\ncontext: res\nattributes:\n  holds: { type: int, range: [0, 5] }\n---\noperation: Resource.delete\ncontext: res\ndefault: allow\nwrites: []\n")
	write("requirements/resources.md", "# Resources\n\n- REQ-001: The system MUST NOT delete a resource with holds.\n")
	s := check(0)
	if s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" {
		t.Fatalf("first requirement: %+v", s)
	}
	// One act: the command that approves, which Claude Code asks about.
	run := "`stele change answer " + s.Change + " " + s.Questions[0].ID + " approve`"
	if !strings.Contains(s.Next, run) || !strings.Contains(s.Next, "Claude Code asks the owner to allow it") || !strings.Contains(s.Next, "do not ask them in chat as well") {
		t.Fatalf("next = %q", s.Next)
	}
	out.Reset()
	if code := cmdChange([]string{"summary", s.Change}, &out); code != 0 || !strings.Contains(out.String(), "Stele read 1 requirement") {
		t.Fatalf("summary: %d %q", code, out.String())
	}
	out.Reset()
	if code := cmdChange([]string{"answer", s.Change, s.Questions[0].ID, "approve", "--asked", "--by", "owner"}, &out); code != 0 || !strings.Contains(out.String(), "approved 1 rule(s)") {
		t.Fatalf("approve: %d\n%s", code, out.String())
	}
	// Nothing new: the same command says so.
	if s := check(0); s.Status != "clean" && s.Status != "approved" {
		t.Fatalf("after the approval: %+v", s)
	}
	// A later requirement that contradicts the approved one is a question.
	write("requirements/resources.md", "# Resources\n\n- REQ-001: The system MUST NOT delete a resource with holds.\n- REQ-002: Erasure requests MUST always delete the resource.\n")
	if s := check(1); s.Status != "questions" || s.Questions[0].Kind != "conflict" {
		t.Fatalf("contradiction: %+v", s)
	}

	// With the prompt off, or no hook, the agent asks in chat.
	if !approvalPrompt(dir) {
		t.Fatal("init installed the hook, and the prompt is not seen")
	}
	t.Setenv("STELE_APPROVAL_PROMPT", "off")
	if approvalPrompt(dir) || approvalPrompt(t.TempDir()) {
		t.Fatal("a prompt is claimed where none is asked")
	}
}

// A requirement Stele finds no rule for still reaches the owner: the check
// reads it once more, then asks for the approval that records it unchecked,
// and after it the requirement is no longer new. The summary names it by
// its own line and does not also call it checked (a trial, 2026-11-09).
func TestRequirementWithNoRuleReachesTheOwner(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	t.Chdir(dir)
	t.Setenv("STELE_APPROVAL_PROMPT", "off")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	var out bytes.Buffer
	if code := cmdInit([]string{".", "--yes", "--harness", "claude"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("init: %d\n%s", code, out.String())
	}
	for rel, src := range map[string]string{
		"requirements/lobby.md": "# Lobby\n\n- REQ-001: The lobby SHOULD feel welcoming.\n",
		"reply.sh": "in=$(cat)\ncase \"$in\" in\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"feel welcoming","status":"uncovered","rules":[]}],"broader":[]}]}' ;;` + "\n" +
			`*'lobby SHOULD'*) echo '{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"informal","rationale":"a feeling, nothing the code can be checked for"}]}' ;;` + "\n" +
			"*) echo '{}' ;;\nesac\n",
	} {
		os.MkdirAll(filepath.Dir(rel), 0o755)
		if err := os.WriteFile(rel, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	check := func() (changeSummary, int) {
		t.Helper()
		var out bytes.Buffer
		code := cmdChange([]string{"markdown", ".", "--json", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}, &out)
		var s changeSummary
		if err := json.Unmarshal(out.Bytes(), &s); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		return s, code
	}
	if s, _ := check(); s.Status != "failed" || !strings.Contains(s.Next, "run the same check again") {
		t.Fatalf("first reading with no rule: %+v", s)
	}
	s, code := check()
	if code != 0 || s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" {
		t.Fatalf("read so again: exit %d, %+v", code, s)
	}
	for _, bad := range []string{"Section:", "checked in part", "Nothing contradicts", "agree"} {
		if strings.Contains(s.Summary, bad) {
			t.Errorf("summary says %q of a requirement with no rule: %s", bad, s.Summary)
		}
	}
	if !strings.Contains(s.Summary, `Stele cannot check "REQ-001: The lobby SHOULD feel welcoming." and would record it unchecked`) {
		t.Errorf("summary = %q", s.Summary)
	}
	if q := s.Questions[0].Text; strings.Contains(q, "retires 0") || strings.Contains(q, "Section:") || !strings.Contains(q, "records its requirements unchecked") {
		t.Errorf("approval question = %q", q)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", s.Change, s.Questions[0].ID, "approve", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("approve: %d\n%s", code, out.String())
	}
	if s, code := check(); code != 0 || (s.Status != "clean" && s.Status != "approved") {
		t.Fatalf("after the approval the requirement is still new: exit %d, %+v", code, s)
	}
}

// A project with specs in a spec tool and nothing approved is adopted
// first: every message names that one command, and a change check says so
// rather than reading the whole project as one change.
func TestChangeBeforeAdoptionNamesAdopt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for rel, src := range map[string]string{
		"openspec/specs/res/spec.md":               "# Resources\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds.\n",
		"openspec/changes/erase/specs/res/spec.md": "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n",
	} {
		os.MkdirAll(filepath.Dir(rel), 0o755)
		if err := os.WriteFile(rel, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := cmdInit([]string{".", "--yes", "--harness", "claude"}, strings.NewReader(""), &out, false); code != 0 || !strings.Contains(out.String(), "adopt our specs with Stele") {
		t.Fatalf("init: %d\n%s", code, out.String())
	}
	cfg, err := config.Find(".")
	if err != nil || !unadopted(cfg, ".") || nextCommand(cfg, ".") != "stele adopt --json" {
		t.Fatalf("not seen as waiting for adoption: %+v, %v", cfg, err)
	}
	out.Reset()
	code := cmdChange([]string{"openspec", ".", "erase", "--json"}, &out)
	var s struct{ Status, Error, Next string }
	if err := json.Unmarshal(out.Bytes(), &s); err != nil || code != 1 || s.Status != "failed" || !strings.Contains(s.Next, "stele adopt --json") {
		t.Fatalf("change before adoption: exit %d, %+v, %v\n%s", code, s, err, out.String())
	}
}

// Without Claude Code, init says what is missing and what still runs, and
// does not end by sending the owner to an agent it did not set up.
func TestInitWithoutAnAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	if code := cmdInit([]string{dir, "--yes"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("init: %d\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "Claude Code was not found here") || !strings.Contains(got, "stele init --harness claude") || !strings.Contains(got, "only the commit check runs") ||
		strings.Contains(got, "open Claude Code and work as usual") || strings.Contains(got, "adopt our specs") || strings.Contains(got, "already set up for") {
		t.Fatalf("init without an agent:\n%s", got)
	}
}
