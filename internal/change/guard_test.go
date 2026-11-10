package change

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// An error case comes first, then a check; neither when both or neither
// are one.
func TestGuardOf(t *testing.T) {
	ids := []string{"a", "b"}
	for _, c := range []struct{ a, b, want string }{
		{"WHEN a member presents a card THEN the system SHALL verify the card and print a receipt", "WHEN a card of a deleted account is used THEN the system SHALL show a message", "b"},
		{"The system SHALL verify that the member exists before lending", "IF the book is on the shelf, the desk SHALL record a new loan", "a"},
		{"WHEN the catalogue lookup fails THEN show only the shelf number", "WHEN the book has a record THEN look it up", "a"},
		{"WHEN a book comes back late THEN set the status to RETURNED_LATE", "WHEN a book comes back THEN set the status to RETURNED", ""},
		{"WHEN the card is invalid THEN show an error", "WHEN the card is expired THEN show an expiry message", ""},
		{"The system SHALL lend a book. Glossary: Error: a failed step", "The system SHALL record the loan", ""},
	} {
		if got := guardOf(c.a, c.b, ids); got != c.want {
			t.Errorf("guardOf(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// An agent's note reaches the translation as a reviewer's, never as the
// owner's word, and with the warning not to write it into the rule.
func TestAgentNoteIsNotTheOwners(t *testing.T) {
	s := translate.Statement{ID: "a", Prose: "An expired card shows an expiry message."}
	h := translate.TextHash(s.Prose)
	agent := notes([]Answer{{Kind: "conflict", Choice: "misread", By: "agent", Note: "the desk never lends on an expired card.", Statements: map[string]string{"a": h}}}, s)
	if len(agent) != 1 || strings.Contains(agent[0], "the owner says") || !strings.Contains(agent[0], "a reviewer (not the owner)") || !strings.Contains(agent[0], "add no condition or outcome the text does not state") {
		t.Fatalf("an agent's note: %v", agent)
	}
	owner := notes([]Answer{{Kind: "conflict", Choice: "misread", By: "Ada", Note: "it is about the message only", Statements: map[string]string{"a": h}}}, s)
	if len(owner) != 1 || !strings.Contains(owner[0], "the owner says") {
		t.Fatalf("the owner's note: %v", owner)
	}
}

// A step's history reads as words in a situation.
func TestPlainSituationHistory(t *testing.T) {
	if got := ran.ReplaceAllString(notRan.ReplaceAllString("card: not ran(verify), ran(scan)", "$1 has not run"), "$1 has run"); got != "card: verify has not run, scan has run" {
		t.Fatalf("history in words: %q", got)
	}
}

// A step that writes one new attribute writes the other new attributes a
// state invariant ties to it on the same entity, also when that entity is
// not the step's own: at an adoption every attribute is new.
func TestStateInvariantWritesAmongNewAttributes(t *testing.T) {
	dir := t.TempDir()
	src := "context: c\nformatVersion: 1\n---\nentity: Receipt\ncontext: c\nattributes:\n  kind: { type: enum, values: [loan, refusal] }\n  listsDueDate: { type: bool }\n  paper: { type: enum, values: [a4, roll] }\n---\nentity: Loan\ncontext: c\nattributes:\n  open: { type: bool }\n---\noperation: Loan.lend\ncontext: c\ndefault: allow\nwrites: [Loan.open, Receipt.kind]\n---\noperation: Loan.close\ncontext: c\ndefault: allow\nwrites: [Loan.open]\n"
	if err := os.WriteFile(filepath.Join(dir, "v.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := vocab.Proposal{Entities: []vocab.Entity{{Name: "Receipt", Attributes: []vocab.Attribute{{Name: "kind"}, {Name: "listsDueDate"}, {Name: "paper"}}}}}
	cands := []translate.Candidate{{Outcome: "rule", Kind: ir.Invariant, Subject: "Receipt", RuleID: "r1", When: "self.kind = loan", Assert: "self.listsDueDate"}}
	got := inferWrites(reg, p, cands)
	if strings.Join(got["Loan.lend"], ",") != "Receipt.listsDueDate" || len(got["Loan.close"]) != 0 {
		t.Fatalf("writes = %v; want Loan.lend alone to write Receipt.listsDueDate", got)
	}
}
