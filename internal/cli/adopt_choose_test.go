package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A bare adoption of a large project reads nothing: it answers with the
// specs to choose from and what each takes. --all reads everything.
func TestAdoptionOfALargeProjectOffersItsSpecs(t *testing.T) {
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
	spec := func(name string, n int) string {
		var b strings.Builder
		b.WriteString("# " + name + "\n\n")
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "### Requirement: R%d\nThe %s desk MUST keep rule %d.\n\n", i, name, i)
		}
		return b.String()
	}
	for rel, src := range map[string]string{
		"openspec/specs/loans/spec.md":   spec("loans", 70),
		"openspec/specs/returns/spec.md": spec("returns", 40),
		"reply.sh":                       "echo called >> " + filepath.Join(dir, "calls.txt") + "\necho '{}'\n",
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
	out.Reset()
	cmdChange([]string{adoptChange, ".", "--json", "--no-code", "--llm", "cmd", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}, &out)
	var s changeSummary
	if err := json.Unmarshal(out.Bytes(), &s); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if s.Status != "choose" || len(s.Specs) != 2 || s.Specs[0].Name != "loans" || s.Specs[0].Requirements != 70 || s.Specs[0].Minutes == 0 || !strings.Contains(s.Summary, "110 requirements in 2 specs") || !strings.Contains(s.Next, "--spec") {
		t.Fatalf("a bare adoption of 110 requirements: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls.txt")); err == nil {
		t.Fatal("the model was called before the owner chose")
	}
	out.Reset()
	cmdChange([]string{adoptChange, ".", "--json", "--no-code", "--all", "--llm", "cmd", "--llm-cmd", "sh " + filepath.Join(dir, "reply.sh")}, &out)
	s = changeSummary{}
	_ = json.Unmarshal(out.Bytes(), &s)
	if s.Status == "choose" {
		t.Fatalf("--all was offered the choice again: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "calls.txt")); err != nil {
		t.Fatal("--all read nothing")
	}
}
