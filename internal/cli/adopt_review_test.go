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

// At an adoption the texts are reviewed too. A contradiction the review
// names and the rules do not show has its requirements read once more,
// told what the review saw; still unshown, it is listed for the owner as
// possible, and the review is not run again.
func TestAdoptionListsWhatTheRulesDoNotShow(t *testing.T) {
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
		"openspec/specs/orders/spec.md": "# Orders\n\n### Requirement: Paid\nAn order MUST be paid.\n\n### Requirement: Gift\nA gift order costs nothing.\n",
		"reply.sh": "in=$(cat)\necho x >> calls.txt\ncase \"$in\" in\n" +
			`*'You review software requirements for contradictions'*) echo review >> calls.txt; echo '{"conflicts":[{"statement_ids":["orders/Paid","orders/Gift"],"world_facts":[],"situation":"a gift order must be paid and costs nothing"}]}' ;;` + "\n" +
			`*'read by their texts alone'*) echo steered >> calls.txt; echo '{"entities":[{"name":"Order","statement_ids":["s1","s2"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"},{"name":"gift","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"","on":"","when":"","assert":"order.paid","overrides":[]},{"statement_id":"s2","kind":"invariant","subject":"","on":"","when":"","assert":"order.gift","overrides":[]}]}' ;;` + "\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*'statement_id: s1'*) echo '{"entities":[{"name":"Order","statement_ids":["s1","s2"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"},{"name":"gift","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"","on":"","when":"","assert":"order.paid","overrides":[]},{"statement_id":"s2","kind":"invariant","subject":"","on":"","when":"","assert":"order.gift","overrides":[]}]}' ;;` + "\n" +
			"*) echo '{}' ;;\nesac\n",
	} {
		os.MkdirAll(filepath.Dir(rel), 0o755)
		if err := os.WriteFile(rel, []byte(strings.ReplaceAll(src, "calls.txt", filepath.Join(dir, "calls.txt"))), 0o644); err != nil {
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
	if s.Status != "ready" || len(s.Possible) != 1 || len(s.Possible[0].Titles) != 2 || !strings.HasPrefix(s.Possible[0].Titles[0], "orders: ") || !strings.Contains(s.Possible[0].Situation, "gift order") {
		t.Fatalf("a contradiction the texts suggest and the rules do not show: %+v", s)
	}
	if !strings.Contains(s.Summary, "may contradict each other") {
		t.Fatalf("the summary does not say so: %s", s.Summary)
	}
	calls, _ := os.ReadFile("calls.txt")
	if strings.Count(string(calls), "review") != 2 || strings.Count(string(calls), "steered") == 0 {
		t.Fatalf("want two reviews and a steered reading, got:\n%s", calls)
	}
	if s = adopt(); len(s.Possible) != 1 {
		t.Fatalf("checked again: %+v", s)
	}
	if again, _ := os.ReadFile("calls.txt"); strings.Count(string(again), "review") != 2 {
		t.Fatalf("the texts were reviewed again:\n%s", again)
	}
}
