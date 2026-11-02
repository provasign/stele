package score

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/translate"
)

func TestConfirm(t *testing.T) {
	dir := t.TempDir()
	reg := `
context: c
formatVersion: 1
---
entity: Change
context: c
attributes:
  stalled: { type: bool }
---
id: A
kind: invariant
assert: change.stalled = true
---
id: B
kind: invariant
assert: change.stalled = false
---
id: C
kind: informal
---
id: E
kind: invariant
assert: change.stalled = true
`
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var stmts []translate.Statement
	for _, id := range []string{"A", "B", "C", "D", "E"} {
		stmts = append(stmts, translate.Statement{ID: id, Source: "spec.md#" + id})
	}
	findings := []Finding{{Sources: []string{"spec.md#A", "spec.md#B"}}}
	rv := &review.Result{Findings: []review.Finding{
		{StatementIDs: []string{"A", "B"}},      // the checker's finding exactly
		{StatementIDs: []string{"A", "B", "C"}}, // the conflict lies within it
		{StatementIDs: []string{"B", "C"}},      // C is informal
		{StatementIDs: []string{"A", "D"}},      // D was never translated
		{StatementIDs: []string{"A", "E"}},      // both rules, and they agree
	}}
	want := []string{Confirmed, Confirmed, Unproved, Unproved, NotReproduced}
	got := Confirm(rv, r, findings, stmts)
	for i, c := range got {
		if c.Verdict != want[i] {
			t.Errorf("candidate %v: %s, want %s (unmodeled %v)", c.Statements, c.Verdict, want[i], c.Unmodeled)
		}
	}
	if cs := Candidates(rv, stmts); len(cs) != 5 || cs[0].ID != "A" {
		t.Errorf("candidates = %v, want A..E in corpus order", cs)
	}
}
