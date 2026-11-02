package touch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/codegraph"
	"github.com/provasign/stele/internal/ir"
)

const vocab = `context: lib
formatVersion: 1
---
entity: Member
context: lib
attributes:
  booksOnLoan: { type: int, range: [0, 20] }
  fines: { type: int, range: [0, 100] }
---
operation: Member.borrow
context: lib
default: allow
writes: [Member.booksOnLoan]
---
operation: Member.pay
context: lib
default: allow
writes: [Member.fines]
`

const rules = `id: limit
kind: precondition
operation: Member.borrow
when: self.booksOnLoan < 5
source: { statementId: intent/s1, doc: .shale/s1.yaml }
prose: "Limit loans\nA member may never have more than 5 books on loan."
---
id: fines
kind: invariant
assert: self.fines <= 100
subject: Member
source: { statementId: intent/s2, doc: .shale/s2.yaml }
prose: Fines never exceed 100.
`

func registry(t *testing.T) *ir.Registry {
	t.Helper()
	dir := t.TempDir()
	for name, src := range map[string]string{"vocabulary.yaml": vocab, "rules.yaml": rules} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestRulesMatchStepsAndAttributes(t *testing.T) {
	reg := registry(t)
	cases := []struct {
		name    string
		changed []codegraph.Symbol
		want    string // the touched statements
		why     string // a piece of the evidence
	}{
		{"the step itself", []codegraph.Symbol{{Name: "Borrow", Parent: "", Kind: "function", File: "lib.go", Line: 3}}, "intent/s1", "is the step Member.borrow"},
		{"a method of the entity", []codegraph.Symbol{{Name: "borrow", Qualified: "Member.borrow", Parent: "Member", Kind: "method", File: "m.py", Line: 9}}, "intent/s1", "is the step"},
		{"a method of another type is not the step", []codegraph.Symbol{{Name: "Borrow", Parent: "Library", Kind: "method", File: "lib.go", Line: 3}}, "", ""},
		{"a field the rule reads", []codegraph.Symbol{{Name: "books_on_loan", Kind: "field", File: "m.py", Line: 2}}, "intent/s1", "is booksOnLoan"},
		{"a getter", []codegraph.Symbol{{Name: "getFines", Kind: "method", File: "M.java", Line: 4}}, "intent/s2", "computes fines"},
		{"code that writes the attribute", []codegraph.Symbol{{Name: "Charge", Kind: "function", File: "f.go", Line: 1, Attrs: []string{"m.Fines"}}}, "intent/s2", "reads or writes fines"},
		{"a caller of the step", []codegraph.Symbol{{Name: "Checkout", Kind: "function", File: "h.go", Line: 7, Calls: []string{"svc.Borrow"}}}, "intent/s1", "calls the step"},
		{"unrelated code", []codegraph.Symbol{{Name: "Greet", Kind: "function", File: "g.go", Line: 1, Calls: []string{"fmt.Println"}}}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Rules(reg, c.changed)
			var ids, ev []string
			for _, x := range got {
				ids = append(ids, x.Statement)
				ev = append(ev, x.Evidence...)
			}
			if strings.Join(ids, ",") != c.want {
				t.Fatalf("touched %v, want %q (evidence %v)", ids, c.want, ev)
			}
			if c.why != "" && !strings.Contains(strings.Join(ev, "\n"), c.why) {
				t.Fatalf("evidence %v lacks %q", ev, c.why)
			}
		})
	}
}

func TestSummarySaysTouchingIsNotBreaking(t *testing.T) {
	ts := Rules(registry(t), []codegraph.Symbol{{Name: "Borrow", Kind: "function", File: "lib.go", Line: 3}})
	s := Summary(ts, 8, func(p string) string { return strings.Join(strings.Fields(p), " ") })
	for _, want := range []string{"touches 1 approved requirement", "(.shale/s1.yaml) [not yet approved by the owner]", "because Borrow (lib.go:3) is the step Member.borrow", "did not check the code"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	if Summary(nil, 8, nil) != "" {
		t.Error("nothing touched should print nothing")
	}
}
