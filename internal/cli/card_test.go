package cli

import (
	"testing"

	"github.com/provasign/stele/internal/card"
	"github.com/provasign/stele/internal/change"
)

// TestDecisionLeavesOutTheExample: the card records which requirements
// clashed and the answer, without the solver's example situation.
func TestDecisionLeavesOutTheExample(t *testing.T) {
	repo := t.TempDir()
	if err := card.Save(repo, &card.Card{Change: "orders"}); err != nil {
		t.Fatal(err)
	}
	ex := `order: ship runs and order: carrier is "a"`
	q := change.Question{Kind: "conflict", Example: ex,
		Text: `"Orders ship within 2 days" (current spec) and "Orders ship within 3 days" (added in this change) cannot both hold, for example when ` + ex + `. Which is right?`}
	if err := recordDecision(repo, "orders", q, "The earlier request is out of date: retire it", "owner"); err != nil {
		t.Fatal(err)
	}
	c, err := card.Load(repo, "orders")
	if err != nil || len(c.Decisions) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	if want := `"Orders ship within 2 days" (current spec) and "Orders ship within 3 days" (added in this change) cannot both hold. Which is right?`; c.Decisions[0].Question != want {
		t.Fatalf("decision = %q, want %q", c.Decisions[0].Question, want)
	}
}
