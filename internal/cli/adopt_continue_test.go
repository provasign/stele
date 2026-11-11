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

// A check an agent runs stops itself before the agent's command limit,
// keeps its work and says to run it again; run again, it finishes.
func TestAdoptionStopsItselfAndContinues(t *testing.T) {
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
		"openspec/specs/orders/spec.md": "# Orders\n\n### Requirement: Mode\nEvery order ships in fast mode.\n",
		"reply.sh": "in=$(cat)\nif [ -n \"$SLOW\" ]; then sleep 3; fi\ncase \"$in\" in\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*'statement_id: s1'*) echo '{"entities":[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"mode","type":"enum","values":["fast","slow"],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"Order","on":"","when":"","assert":"self.mode = fast","overrides":[]}]}' ;;` + "\n" +
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
	t.Setenv("STELE_SLICE", "1s")
	t.Setenv("SLOW", "1")
	s := adopt()
	if s.Status != "continue" || !strings.Contains(s.Next, "run the same command again") {
		t.Fatalf("a check that outlasts its slice: %+v", s)
	}
	t.Setenv("STELE_SLICE", "")
	t.Setenv("SLOW", "")
	if s = adopt(); s.Status != "ready" {
		t.Fatalf("run again: %+v", s)
	}
}
