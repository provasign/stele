package cli

import (
	"bytes"
	"encoding/json"
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
	rule := func(effect, when string) string {
		return `{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"permission","operation":"Resource.delete","effect":"` + effect + `","when":"` + when + `","overrides":[]}]}`
	}
	write("reply.sh", "in=$(cat)\ncase \"$in\" in\n"+
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
	rule := func(effect, when string) string {
		return `{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"permission","operation":"Resource.delete","effect":"` + effect + `","when":"` + when + `","overrides":[]}]}`
	}
	write("reply.sh", "in=$(cat)\ncase \"$in\" in\n"+
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
