// Package card writes Stele's part of a pull request's evidence: one small
// committed file per change, .stele/cards/<change>.json, that a PR card
// (Shale's) renders without running Stele. It says what the change does to
// the approved requirements, what the owner decided, and which approved
// requirements the branch's code touches.
package card

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dir holds the cards, relative to the repository root.
const Dir = ".stele/cards"

// Version is the card format this Stele writes; a reader rejects a newer
// major version and ignores fields it does not know.
const Version = 1

// Card is one change's evidence.
type Card struct {
	Version int    `json:"stele_card"`
	Change  string `json:"change"`
	Format  string `json:"format,omitempty"` // shale, speckit, kiro, openspec; empty for adoption
	Base    string `json:"base,omitempty"`
	// Status is the latest check's: approved, ready (only the owner's
	// approval is left), questions (decisions are open), clean (nothing to
	// decide), broken (the rules do not compile together), or failed (the
	// model could not translate requirements, so nothing was checked).
	Status string `json:"status"`
	// Checked is how many approved requirements the change was checked
	// against.
	Checked      int           `json:"checked_against"`
	Requirements []Requirement `json:"requirements,omitempty"`
	Decisions    []Decision    `json:"decisions,omitempty"`
	Open         int           `json:"open_questions"`
	ApprovedBy   string        `json:"approved_by,omitempty"`
	ApprovedAt   string        `json:"approved_at,omitempty"`
	// Touched are the approved requirements the branch's code touches, as
	// of Updated: names matched, not the code checked against the rules.
	Touched []Touched `json:"touched,omitempty"`
	Updated string    `json:"updated"`
}

// Requirement is one requirement the change adds, edits or retires.
type Requirement struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Change  string   `json:"change"` // added | modified | retired
	Source  string   `json:"source,omitempty"`
	Reading []string `json:"reading,omitempty"` // Stele's rules, one per line
}

// Decision is one of the owner's answers.
type Decision struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
	By       string `json:"by"`
	At       string `json:"at"`
}

// Touched is an approved requirement the code touches, and why.
type Touched struct {
	Statement string   `json:"statement"`
	Title     string   `json:"title"`
	Evidence  []string `json:"evidence"`
}

// Path is where change's card lives under repo.
func Path(repo, change string) string {
	return filepath.Join(repo, Dir, safe(change)+".json")
}

// Load reads change's card; a missing card is nil with no error.
func Load(repo, change string) (*Card, error) {
	src, err := os.ReadFile(Path(repo, change))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Card
	if err := json.Unmarshal(src, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(repo, change), err)
	}
	return &c, nil
}

// Save writes the card, stamped with the time.
func Save(repo string, c *Card) error {
	c.Version = Version
	c.Updated = time.Now().UTC().Format(time.RFC3339)
	js, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	p := Path(repo, c.Change)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(js, '\n'), 0o644)
}

// Title is how a card names a statement: its first line, shortened.
func Title(prose, id string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(prose), "\n")
	line = strings.TrimSpace(strings.TrimPrefix(line, "Requirement:"))
	if line == "" {
		return id
	}
	if r := []rune(line); len(r) > 90 {
		line = string(r[:87]) + "..."
	}
	return line
}

func safe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), ".")
}
