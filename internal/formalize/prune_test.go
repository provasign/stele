package formalize

import (
	"slices"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// TestPruneKeepsWhatFailingRulesName: a rule that does not compile yet
// keeps the symbols it names. A loan history read only through
// instances(LoanRecord) lost its entity and operation to pruning, so it
// could never compile (a developer trial, 2026-11-03).
func TestPruneKeepsWhatFailingRulesName(t *testing.T) {
	p := vocab.Proposal{
		Entities: []vocab.Entity{
			{Name: "LoanRecord", Attributes: []vocab.Attribute{{Name: "toolId", Type: "int"}, {Name: "unused", Type: "int"}}},
			{Name: "Spare", Attributes: []vocab.Attribute{{Name: "x", Type: "int"}}},
		},
		Extensions: []vocab.Entity{{Name: "Tool", Relations: []vocab.Relation{{Name: "lastLoan", To: "LoanRecord"}, {Name: "spare", To: "Spare"}}}},
		Operations: []vocab.Operation{{Name: "Tool.history"}, {Name: "Tool.idle"}},
	}
	cands := []translate.Candidate{{Outcome: "rule", Kind: ir.Invariant, On: "Tool.history",
		Assert: "all(r in instances(LoanRecord): r.toolId = self.id)", Error: "instances(LoanRecord): no such entity"},
		{Outcome: "rule", Kind: ir.Invariant, On: "Tool.return", Assert: "self.lastLoan.toolId = self.id", Error: `Tool has no attribute or relation "lastLoan"`}}
	_, pruned := prune(&ir.Registry{Entities: map[string]*ir.Entity{}, Operations: map[string]*ir.Operation{}}, p, nil, named(cands))
	for _, kept := range []string{"entity LoanRecord", "attribute LoanRecord.toolId", "operation Tool.history", "relation Tool.lastLoan"} {
		if slices.Contains(pruned, kept) {
			t.Errorf("pruned %s, which a failing rule names", kept)
		}
	}
	for _, gone := range []string{"attribute LoanRecord.unused", "operation Tool.idle", "relation Tool.spare"} {
		if !slices.Contains(pruned, gone) {
			t.Errorf("kept %s; pruned %v", gone, pruned)
		}
	}
}
