package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A project's specs can be adopted a part at a time: the first part is
// approved with the others named as not read yet, a later part is read
// against what is approved, and the registry records which were read.
func TestAdoptionBySpec(t *testing.T) {
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
	entity := func(name, attr string) string {
		return `{"entities":[{"name":"` + name + `","statement_ids":["s1"],"attributes":[{"name":"` + attr + `","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"` + name + `","on":"","when":"","assert":"self.` + attr + `","overrides":[]}]}`
	}
	for rel, src := range map[string]string{
		"openspec/specs/orders/spec.md":  "# Orders\n\n### Requirement: Paid\nAn order MUST be paid.\n",
		"openspec/specs/returns/spec.md": "# Returns\n\n### Requirement: Approved\nA return MUST be approved.\n",
		"reply.sh": "in=$(cat)\ncase \"$in\" in\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*'An order MUST be paid'*) echo '` + entity("Order", "paid") + `' ;;` + "\n" +
			`*'A return MUST be approved'*) echo '` + entity("Return", "approved") + `' ;;` + "\n" +
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
	adopt := func(spec string) (changeSummary, string) {
		t.Helper()
		var out bytes.Buffer
		args := []string{adoptChange, ".", "--json", "--no-code", "--llm", "cmd", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}
		if spec != "" {
			args = append(args, "--spec", spec)
		}
		cmdChange(args, &out)
		var s changeSummary
		_ = json.Unmarshal(out.Bytes(), &s)
		return s, out.String()
	}
	approve := func(s changeSummary) {
		t.Helper()
		var out bytes.Buffer
		if s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" {
			t.Fatalf("not ready for approval: %+v", s)
		}
		if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "approve", "--by", "owner"}, &out); code != 0 {
			t.Fatalf("approve: %d\n%s", code, out.String())
		}
	}
	out.Reset()
	if code := cmdChange([]string{adoptChange, ".", "--list"}, &out); code != 0 || !strings.Contains(out.String(), "orders\t1 requirement(s)\tnot read yet") || !strings.Contains(out.String(), "returns\t1 requirement(s)\tnot read yet") {
		t.Fatalf("the list of specs: %d\n%s", code, out.String())
	}
	if s, raw := adopt("nosuch"); s.Status != "" && s.Status != "failed" {
		t.Fatalf("a spec that does not exist was adopted: %s", raw)
	}
	s, _ := adopt("orders")
	if !strings.Contains(s.Summary, "Stele read 1 requirement") || !strings.Contains(s.Summary, "1 other spec is not read yet (returns)") {
		t.Fatalf("the first part's summary: %s", s.Summary)
	}
	approve(s)
	reg := filepath.Join(".stele", "registry")
	if src, err := os.ReadFile(filepath.Join(reg, "adopted-specs.txt")); err != nil || strings.TrimSpace(string(src)) != "orders" {
		t.Fatalf("the specs read are not recorded: %q %v", src, err)
	}
	if _, err := os.Stat(filepath.Join(reg, "adopted-orders.yaml")); err != nil {
		t.Fatalf("the first part's rules: %v", err)
	}
	// With part of the specs read, a bare adopt says what is left.
	var errOut bytes.Buffer
	stderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := cmdChange([]string{adoptChange, ".", "--json", "--no-code", "--llm", "cmd", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}, &errOut)
	w.Close()
	os.Stderr = stderr
	msg, _ := io.ReadAll(r)
	if code == 0 || !strings.Contains(string(msg)+errOut.String(), "not read yet: returns") {
		t.Fatalf("a bare adopt with a part left: %d %s %s", code, msg, errOut.String())
	}
	s, _ = adopt("returns")
	if !strings.Contains(s.Summary, "Stele read 1 requirement") || strings.Contains(s.Summary, "not read yet") {
		t.Fatalf("the second part's summary: %s", s.Summary)
	}
	approve(s)
	if src, _ := os.ReadFile(filepath.Join(reg, "adopted-specs.txt")); strings.Join(strings.Fields(string(src)), ",") != "orders,returns" {
		t.Fatalf("the specs read after the second part: %q", src)
	}
	for _, f := range []string{"adopted-orders.yaml", "adopted-returns.yaml"} {
		if _, err := os.Stat(filepath.Join(reg, f)); err != nil {
			t.Fatalf("after the second part: %v", err)
		}
	}
	out.Reset()
	if code := cmdCheck([]string{reg}, &out); code != 0 || !strings.Contains(out.String(), "2 rule(s)") {
		t.Fatalf("check of the registry adopted in two parts: %d\n%s", code, out.String())
	}
	out.Reset()
	if cmdChange([]string{adoptChange, ".", "--list"}, &out); !strings.Contains(out.String(), "orders\t1 requirement(s)\tread\n") || strings.Contains(out.String(), "not read yet") {
		t.Fatalf("the list of specs after both parts:\n%s", out.String())
	}
	if s, raw := adopt("orders"); s.Status != "" && s.Status != "failed" {
		t.Fatalf("a spec already read was adopted again: %s", raw)
	}
}

// A spec adopted after another may contradict it: the contradiction can be
// decided later, and is recorded beside the rules of the part that met it.
func TestAdoptionBySpecLaterConflict(t *testing.T) {
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
	entity := func(name, attr string) string {
		return `{"entities":[{"name":"` + name + `","statement_ids":["s1"],"attributes":[{"name":"` + attr + `","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"` + name + `","on":"","when":"","assert":"self.` + attr + `","overrides":[]}]}`
	}
	for rel, src := range map[string]string{
		"openspec/specs/orders/spec.md":  "# Orders\n\n### Requirement: Paid\nAn order MUST be paid.\n",
		"openspec/specs/samples/spec.md": "# Samples\n\n### Requirement: Free\nAn order is never paid.\n",
		"reply.sh": "in=$(cat)\ncase \"$in\" in\n" +
			`*'You review software requirements for contradictions'*) echo '{"conflicts":[{"statement_ids":["orders/Paid","samples/Free"],"world_facts":[],"situation":"the two cannot both hold for one order"}]}' ;;` + "\n" +
			`*'list its clauses'*) echo '{"statements":[{"statement_id":"s1","clauses":[{"text":"all","status":"covered","rules":["r1"]}],"broader":[]}]}' ;;` + "\n" +
			`*'An order is never paid'*) echo '` + strings.Replace(entity("Order", "paid"), `"assert":"self.paid"`, `"assert":"not self.paid"`, 1) + `' ;;` + "\n" +
			`*'An order MUST be paid'*) echo '` + entity("Order", "paid") + `' ;;` + "\n" +
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
	adopt := func(spec string) (changeSummary, string) {
		t.Helper()
		var out bytes.Buffer
		args := []string{adoptChange, ".", "--json", "--no-code", "--llm", "cmd", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}
		if spec != "" {
			args = append(args, "--spec", spec)
		}
		cmdChange(args, &out)
		var s changeSummary
		_ = json.Unmarshal(out.Bytes(), &s)
		return s, out.String()
	}
	approve := func(s changeSummary) {
		t.Helper()
		var out bytes.Buffer
		if s.Status != "ready" || len(s.Questions) != 1 || s.Questions[0].Kind != "approve" {
			t.Fatalf("not ready for approval: %+v", s)
		}
		if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "approve", "--by", "owner"}, &out); code != 0 {
			t.Fatalf("approve: %d\n%s", code, out.String())
		}
	}
	s, _ := adopt("orders")
	approve(s)
	s, _ = adopt("samples")
	if s.Status != "questions" || len(s.Questions) != 1 || s.Questions[0].Kind != "conflict" {
		t.Fatalf("a part that contradicts an approved one: %+v", s)
	}
	var choices []string
	for _, o := range s.Questions[0].Options {
		choices = append(choices, o.Choice)
	}
	if !slices.Contains(choices, "later") || !slices.Contains(choices, "prevails") {
		t.Fatalf("the options across parts: %v", choices)
	}
	out.Reset()
	if code := cmdChange([]string{"answer", adoptChange, s.Questions[0].ID, "later", "--by", "owner"}, &out); code != 0 {
		t.Fatalf("later: %d\n%s", code, out.String())
	}
	s, _ = adopt("samples")
	approve(s)
	reg := filepath.Join(".stele", "registry")
	if src, err := os.ReadFile(filepath.Join(reg, "conflicts", "adoption-samples.yaml")); err != nil || !strings.Contains(string(src), "OC-samples-1") {
		t.Fatalf("the open conflict of the second part: %v\n%s", err, src)
	}
	out.Reset()
	if code := cmdCheck([]string{reg}, &out); code != 0 || !strings.Contains(out.String(), "1 tolerated") {
		t.Fatalf("check of the registry: %d\n%s", code, out.String())
	}
}
