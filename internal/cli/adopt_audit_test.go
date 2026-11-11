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

// A rule in a conflict that does not say what its own text says is set
// aside before the owner is asked: the conflict goes, the requirement is
// recorded without that rule, and the summary says so.
func TestAdoptionSetsAsideAMisreadRule(t *testing.T) {
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
			`*'check each rule against the text'*) id=$(printf '%s\n' "$in" | sed -n 's/^## Conflict //p' | head -1); echo "{\"conflicts\":[{\"id\":\"$id\",\"rules\":[{\"rule\":\"orders/Paid\",\"verdict\":\"says\",\"reason\":\"as written\"},{\"rule\":\"orders/Free\",\"verdict\":\"misread\",\"reason\":\"the text speaks of sample orders only.\"}]}]}" ;;` + "\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*"Stele's first reading of this requirement clashed"*) echo '{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"","on":"","when":"","assert":"not order.paid","overrides":[]}]}' ;;` + "\n" +
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
	if s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" {
		t.Fatalf("adoption with a misread rule in its one conflict: %+v", s)
	}
	if !strings.Contains(s.Summary, "set aside 1 of its own rules") {
		t.Fatalf("the summary does not say a rule was set aside: %s", s.Summary)
	}
	if _, ok := s.Unchecked["orders/Free"]; !ok {
		t.Fatalf("the requirement whose only rule was set aside is not recorded unchecked: %+v", s.Unchecked)
	}
	// The same check again asks the model nothing new and sets nothing more aside.
	if s = adopt(); s.Status != "ready" || !strings.Contains(s.Summary, "set aside 1 of its own rules") {
		t.Fatalf("checked again: %+v", s)
	}
}
