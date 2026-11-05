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

// TestOnlyReportedWrites: an approved listing recorded as writing the
// tool's id and name, which its rule only reports, is found; a value a rule
// sets after the step, and one a state invariant ties to another value the
// step writes, are not.
func TestOnlyReportedWrites(t *testing.T) {
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
  lentAt: { type: int }
  dueAt: { type: int }
---
extend: Result
context: c
attributes:
  nameOf: { type: "map<int, string>" }
  due: { type: int }
---
operation: Tool.list
context: c
default: allow
writes: [Tool.id, Tool.name, Tool.status]
---
operation: Tool.lend
context: c
default: allow
writes: [Tool.status, Tool.lentAt, Tool.dueAt]
---
id: r1
kind: invariant
on: Tool.list
assert: result.nameOf[self.id] = self.name
status: active
---
id: r2
kind: invariant
on: Tool.list
assert: self.status = old(self.status)
status: active
---
id: r3
kind: invariant
on: Tool.lend
assert: self.status = lent and result.due = self.dueAt
status: active
---
id: r4
kind: invariant
subject: Tool
assert: self.dueAt = self.lentAt + 14
status: active
`
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := func(op string) []string {
		var out []string
		for _, a := range OnlyReported(reg, reg.Operations[op]) {
			out = append(out, a.Path())
		}
		return out
	}
	if got := names("Tool.list"); len(got) != 2 || got[0] != "Tool.id" || got[1] != "Tool.name" {
		t.Errorf("the listing only reports %v, want Tool.id, Tool.name", got)
	}
	if got := names("Tool.lend"); len(got) != 0 {
		t.Errorf("lending only reports %v, want none: the due date is tied to the lent date", got)
	}
}
