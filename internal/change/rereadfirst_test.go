package change

import (
	"strings"
	"testing"

	"github.com/provasign/stele/internal/translate"
)

// A conflict the code shows to be none is the agent's to answer once, by
// having Stele read the requirements again; after that it is the owner's.
func TestCodeWithoutClashIsRereadFirst(t *testing.T) {
	text := "A returned book MUST be shelved."
	conflict := func(outcome string) Question {
		return Question{Kind: "conflict", AskOwner: true, Statements: []string{"loans/Shelve"},
			Requirements: []Requirement{{ID: "loans/Shelve", Text: text}, {ID: "loans/Repair", Text: "A damaged book MUST go to repair."}},
			Options:      []Option{{Choice: "change"}, {Choice: "misread", Note: true}},
			Code:         &Code{Outcome: outcome, Says: "In the code the two do not clash: a damaged book is never marked returned."}}
	}
	q := conflict("compatible")
	rereadFirst(&q, nil)
	if q.AskOwner || q.Recommended != "misread" || q.Options[0].Choice != "misread" || !strings.Contains(q.Why, "never marked returned") {
		t.Fatalf("no clash in the code: %+v", q)
	}
	q = conflict("compatible")
	rereadFirst(&q, []Answer{{By: "agent", Choice: "misread", Statements: map[string]string{"loans/Shelve": translate.TextHash(text)}}})
	if !q.AskOwner || q.Recommended != "" {
		t.Fatalf("already read again at the agent's note: %+v", q)
	}
	q = conflict("follows")
	rereadFirst(&q, nil)
	if !q.AskOwner {
		t.Fatalf("the code takes a side: %+v", q)
	}
}

// A question the owner already answered stays the owner's when it still
// clashes: the agent's reread would replace their answer.
func TestRereadFirstLeavesTheOwnersQuestion(t *testing.T) {
	q := &Question{ID: "conflict-1", Kind: "conflict", AskOwner: true, Statements: []string{"a", "b"},
		Requirements: []Requirement{{ID: "a", Text: "A"}, {ID: "b", Text: "B"}},
		Options:      []Option{{Choice: "misread", Note: true}, {Choice: "later"}},
		Code:         &Code{Outcome: "compatible", Says: "the code does both"}}
	if rereadFirst(q, []Answer{{Question: "conflict-1", Kind: "conflict", Choice: "prevails", By: "Ada"}}) || !q.AskOwner {
		t.Fatalf("a question the owner answered went to the agent: %+v", q)
	}
}
