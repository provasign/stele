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

// At an adoption the owner may decide a contradiction later: it is recorded
// as an open conflict, the approval names it, and the adopted registry
// passes its check with it.
func TestAdoptionLeavesAConflictOpen(t *testing.T) {
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
	for rel, src := range map[string]string{
		"openspec/specs/orders/spec.md": "# Orders\n\n### Requirement: Paid\nAn order MUST be paid.\n\n### Requirement: Free\nAn order MUST NOT be paid.\n",
		"reply.sh": "in=$(cat)\ncase \"$in\" in\n" +
			`*'You review software requirements for contradictions'*) echo '{"conflicts":[{"statement_ids":["orders/Paid","orders/Free"],"world_facts":[],"situation":"the two cannot both hold for one order"}]}' ;;` + "\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*'statement_id: s1'*) echo '{"entities":[{"name":"Order","statement_ids":["s1","s2"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"","on":"","when":"","assert":"order.paid","overrides":[]},{"statement_id":"s2","kind":"invariant","subject":"","on":"","when":"","assert":"not order.paid","overrides":[]}]}' ;;` + "\n" +
			"*) echo '{}' ;;\nesac\n",
	} {
		os.MkdirAll(filepath.Dir(rel), 0o755)
		if err := os.WriteFile(rel, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := cmdInit([]string{".", "--yes", "--harness", "claude"}, strings.NewReader(""), &out, false); code != 0 {
		t.Fatalf("init: %d\n%s", code, out.String())
	}
	adopt := func() changeSummary {
		t.Helper()
		var out bytes.Buffer
		cmdChange([]string{adoptChange, ".", "--json", "--no-code", "--llm", "cmd", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}, &out)
		var s changeSummary
		if err := json.Unmarshal(out.Bytes(), &s); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		return s
	}
	s := adopt()
	if s.Status != "questions" || len(s.Questions) != 1 || s.Questions[0].Kind != "conflict" {
		t.Fatalf("adoption of two requirements that cannot both hold: %+v", s)
	}
	later := false
	for _, o := range s.Questions[0].Options {
		later = later || o.Choice == "later"
	}
	if !later {
		t.Fatalf("the conflict cannot be decided later: %+v", s.Questions[0].Options)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "later", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("later: %d\n%s", code, out.String())
	}
	s = adopt()
	if s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" || !strings.Contains(s.Questions[0].Text, "decide later stays open") {
		t.Fatalf("after deciding later: %+v", s)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "approve", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("approve: %d\n%s", code, out.String())
	}
	src, err := os.ReadFile(filepath.Join(".stele", "registry", "conflicts", "adoption.yaml"))
	if err != nil || !strings.Contains(string(src), "status: open") || !strings.Contains(string(src), "owner") {
		t.Fatalf("the open conflict is not in the registry: %v\n%s", err, src)
	}
	out.Reset()
	if code := cmdCheck([]string{filepath.Join(".stele", "registry")}, &out); code != 0 || !strings.Contains(out.String(), "1 tolerated") {
		t.Fatalf("check of the adopted registry: %d\n%s", code, out.String())
	}
}
