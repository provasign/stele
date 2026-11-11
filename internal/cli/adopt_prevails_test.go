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

// At an adoption, a plain "SHALL" against a check before it is one
// requirement taking precedence over the other, as any reader takes it: the
// check is recommended as the exception, and the answer settles the clash
// without rewording the spec or reading it again.
func TestAdoptionCheckPrevails(t *testing.T) {
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
		"openspec/specs/orders/spec.md": "# Orders\n\n### Requirement: Ship\nThe system SHALL ship an order when it is submitted.\n\n### Requirement: Check\nThe system SHALL validate that the order is paid before shipping it.\n",
		"reply.sh": "in=$(cat)\ncase \"$in\" in\n" +
			`*'You review software requirements for contradictions'*) echo '{"conflicts":[{"statement_ids":["orders/Ship","orders/Check"],"world_facts":[],"situation":"the two cannot both hold for one order"}]}' ;;` + "\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*'statement_id: s1'*) echo '{"entities":[{"name":"Order","statement_ids":["s1","s2"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[{"name":"Order.ship","statement_ids":["s1","s2"],"default":"allow","writes":[],"rationale":""}],"rules":[{"statement_id":"s1","kind":"permission","operation":"Order.ship","effect":"allow","when":"","overrides":[]},{"statement_id":"s2","kind":"precondition","operation":"Order.ship","when":"self.paid","overrides":[]}]}' ;;` + "\n" +
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
		t.Fatalf("adoption of a must-run and a check: %+v", s)
	}
	q := s.Questions[0]
	if q.Recommended != "prevails" {
		t.Fatalf("the check is not recommended as the exception: %+v", q)
	}
	var win []string
	for _, o := range q.Options {
		if o.Choice == "prevails" {
			win = o.Prevails
		}
	}
	if len(win) != 1 || !strings.HasSuffix(win[0], "Check") {
		t.Fatalf("the recommended exception is %q, not the check: %+v", win, q.Options)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", adoptChange, q.ID, "prevails", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("prevails: %d\n%s", code, out.String())
	}
	s = adopt()
	if s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" {
		t.Fatalf("after the check prevails: %+v", s)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "approve", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("approve: %d\n%s", code, out.String())
	}
	out.Reset()
	if code := cmdCheck([]string{filepath.Join(".stele", "registry")}, &out); code != 0 || strings.Contains(out.String(), "tolerated") {
		t.Fatalf("check of the adopted registry: %d\n%s", code, out.String())
	}
}

// A rule that fixes a value makes its other values impossible, as it says:
// the adopted registry passes its own check.
// A clash between Stele's rules that a review of the texts does not back is
// not the owner's question: Stele settles it in the same run, here by
// taking the check as coming first, and the summary and the approval say so.
func TestAdoptionSettlesWhatNoReaderSees(t *testing.T) {
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
		"openspec/specs/orders/spec.md": "# Orders\n\n### Requirement: Ship\nThe system SHALL ship an order when it is submitted.\n\n### Requirement: Check\nThe system SHALL validate that the order is paid before shipping it.\n",
		"reply.sh": "in=$(cat)\ncase \"$in\" in\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*'statement_id: s1'*) echo '{"entities":[{"name":"Order","statement_ids":["s1","s2"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[{"name":"Order.ship","statement_ids":["s1","s2"],"default":"allow","writes":[],"rationale":""}],"rules":[{"statement_id":"s1","kind":"permission","operation":"Order.ship","effect":"allow","when":"","overrides":[]},{"statement_id":"s2","kind":"precondition","operation":"Order.ship","when":"self.paid","overrides":[]}]}' ;;` + "\n" +
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
		t.Fatalf("a clash of the rules the review does not back went to the owner: %+v", s)
	}
	if !strings.Contains(s.Summary, "it settled them itself (0 recorded as open, 1 by taking the check or the error case first)") {
		t.Fatalf("the summary does not say what Stele settled: %s", s.Summary)
	}
	if !strings.Contains(s.Questions[0].Text, "Stele's rules clash in 1 more place(s)") {
		t.Fatalf("the approval does not say what Stele settled: %s", s.Questions[0].Text)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "approve", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("approve: %d\n%s", code, out.String())
	}
	out.Reset()
	if code := cmdCheck([]string{filepath.Join(".stele", "registry")}, &out); code != 0 {
		t.Fatalf("check of the adopted registry: %d\n%s", code, out.String())
	}
}

func TestAdoptionFixedValueChecks(t *testing.T) {
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
		"reply.sh": "in=$(cat)\ncase \"$in\" in\n" +
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
	s := adopt()
	if s.Status != "ready" {
		t.Fatalf("adoption of one fixed value: %+v", s)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "approve", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("approve: %d\n%s", code, out.String())
	}
	out.Reset()
	if code := cmdCheck([]string{filepath.Join(".stele", "registry")}, &out); code != 0 || !strings.Contains(out.String(), "as the rule says") {
		t.Fatalf("check of the adopted registry: %d\n%s", code, out.String())
	}
}
