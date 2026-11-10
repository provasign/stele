package change

import (
	"strings"
	"testing"

	"github.com/provasign/stele/internal/translate"
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
