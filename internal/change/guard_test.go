package change

import "testing"

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
