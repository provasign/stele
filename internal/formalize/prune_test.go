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

// TestPruneKeepsWhatInstancesRead: attributes a rule reads only through
// the variable of instances(E) are used. A project's first requirement,
// "a member must not hold more than 3 books", read Book.status and
// Book.borrowerId that way; both were pruned, the rules stopped compiling,
// and the check called the change clean with nothing to approve (a trial,
// 2026-11-09).
func TestPruneKeepsWhatInstancesRead(t *testing.T) {
	p := vocab.Proposal{
		Entities: []vocab.Entity{
			{Name: "Member", Attributes: []vocab.Attribute{{Name: "id", Type: "string"}}},
			{Name: "Book", Attributes: []vocab.Attribute{{Name: "status", Type: "enum", Values: []string{"available", "lent"}},
				{Name: "borrowerId", Type: "optional<string>"}, {Name: "shelf", Type: "int"}}},
		},
	}
	reg, err := vocab.Load(vocab.Options{Context: "c"}, p)
	if err != nil {
		t.Fatal(err)
	}
	cands := []translate.Candidate{{Outcome: "rule", Kind: ir.Invariant, Subject: "Member", RuleID: "r1",
		Assert: "count(b in instances(Book): b.status = lent and b.borrowerId = self.id) <= 3"}}
	out, pruned := prune(reg, p, parseRules(reg, cands), named(cands))
	for _, kept := range []string{"attribute Book.status", "attribute Book.borrowerId", "entity Book", "attribute Member.id"} {
		if slices.Contains(pruned, kept) {
			t.Errorf("pruned %s, which the rule reads through instances(Book)", kept)
		}
	}
	if !slices.Contains(pruned, "attribute Book.shelf") {
		t.Errorf("kept Book.shelf, which no rule reads; pruned %v", pruned)
	}
	if _, err := vocab.Load(vocab.Options{Context: "c"}, out); err != nil {
		t.Fatal(err)
	}
}

// TestSettleVocabularyKeepsARuleOverPruning: pruning never costs a rule.
// When a rule compiles with the unpruned vocabulary and not with the
// pruned one, the unpruned one is written.
func TestSettleVocabularyKeepsARuleOverPruning(t *testing.T) {
	full := vocab.Proposal{Entities: []vocab.Entity{
		{Name: "Member", Attributes: []vocab.Attribute{{Name: "loans", Type: "int", Max: 9}, {Name: "fines", Type: "int", Max: 9}}},
	}}
	overPruned := vocab.Proposal{Entities: []vocab.Entity{
		{Name: "Member", Attributes: []vocab.Attribute{{Name: "fines", Type: "int", Max: 9}}},
	}}
	cands := func() []translate.Candidate {
		return []translate.Candidate{{Outcome: "rule", Kind: ir.Invariant, Subject: "Member", RuleID: "r1", StatementID: "s1", Assert: "self.loans <= 3"}}
	}
	opt := Options{Context: "c"}
	cs := cands()
	p, _, kept, err := settleVocabulary(t.TempDir(), overPruned, full, cs, opt)
	if err != nil || !kept || cs[0].Error != "" || len(p.Entities[0].Attributes) != 2 {
		t.Fatalf("kept %v, error %q, vocabulary %+v, %v; want the unpruned vocabulary and a rule that compiles", kept, cs[0].Error, p, err)
	}
	// A rule that compiles with neither keeps the pruned vocabulary and its error.
	cs = cands()
	cs[0].Assert = "self.missing <= 3"
	p, _, kept, err = settleVocabulary(t.TempDir(), overPruned, full, cs, opt)
	if err != nil || kept || cs[0].Error == "" || len(p.Entities[0].Attributes) != 1 {
		t.Fatalf("kept %v, error %q, vocabulary %+v, %v; want the pruned vocabulary and the rule's error", kept, cs[0].Error, p, err)
	}
}
