package formalize

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// TestReportedValuesAreNotWrites: a listing that shows each tool's name
// against its id reads both; inferred as writes, they let the listing
// change any tool, and a clash no step reaches was kept (a developer trial,
// 2026-11-04). A value the step sets is still a write.
func TestReportedValuesAreNotWrites(t *testing.T) {
	dir := t.TempDir()
	src := `context: c
formatVersion: 1
---
entity: Tool
context: c
attributes:
  id: { type: int }
  name: { type: string }
  status: { type: enum, values: [available, lent] }
  exists: { type: bool }
---
extend: Result
context: c
attributes:
  nameOf: { type: "map<int, string>" }
  listedIds: { type: list, of: int }
  listedNames: { type: list, of: string }
---
operation: Tool.list
context: c
default: allow
writes: []
---
operation: Tool.lend
context: c
default: allow
writes: []
`
	if err := os.WriteFile(filepath.Join(dir, "vocabulary.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cands := []translate.Candidate{
		{RuleID: "r1", Outcome: "rule", Kind: ir.Invariant, On: "Tool.list", Assert: "has(result.nameOf, self.id) and result.nameOf[self.id] = self.name"},
		// The listing as a later trial read it.
		{RuleID: "r3", Outcome: "rule", Kind: ir.Invariant, On: "Tool.list", Assert: "(self.exists implies contains(result.listedIds, self.id) and contains(result.listedNames, self.name)) and all(t in instances(Tool): t.exists implies contains(result.listedIds, t.id))"},
		{RuleID: "r4", Outcome: "rule", Kind: ir.Invariant, On: "Tool.list", Assert: "all(i in result.listedIds: (self.exists and self.id = i) or any(t in instances(Tool): t.exists and t.id = i))"},
		{RuleID: "r2", Outcome: "rule", Kind: ir.Invariant, On: "Tool.lend", Assert: "self.status = lent"},
	}
	p := vocab.Proposal{Operations: []vocab.Operation{{Name: "Tool.list", Default: "allow"}, {Name: "Tool.lend", Default: "allow"}}}
	out, added := inferWrites(reg, p, parseRules(reg, cands))
	if len(out.Operations[0].Writes) != 0 {
		t.Errorf("the listing writes %v (%v)", out.Operations[0].Writes, added)
	}
	if len(out.Operations[1].Writes) != 1 || out.Operations[1].Writes[0] != "Tool.status" {
		t.Errorf("lending writes %v, want Tool.status", out.Operations[1].Writes)
	}
}
