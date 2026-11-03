package card

import (
	"os"
	"strings"
	"testing"
)

func TestSaveLoad(t *testing.T) {
	repo := t.TempDir()
	if c, err := Load(repo, "feature/summer"); c != nil || err != nil {
		t.Fatalf("no card yet: %v, %v", c, err)
	}
	c := &Card{Change: "feature/summer", Status: "approved", Requirements: []Requirement{{ID: "intent/s2", Title: Title("Summer reading program\nup to 10 books", "intent/s2"), Change: "added"}}}
	if err := Save(repo, c); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(Path(repo, c.Change), ".stele/cards/feature-summer.json") {
		t.Fatalf("path = %s", Path(repo, c.Change))
	}
	got, err := Load(repo, "feature/summer")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Version != Version || got.Updated == "" || got.Requirements[0].Title != "Summer reading program" {
		t.Fatalf("card = %+v", got)
	}
	src, _ := os.ReadFile(Path(repo, c.Change))
	if !strings.Contains(string(src), `"stele_card": 1`) {
		t.Fatalf("written card lacks its version:\n%s", src)
	}
}

func TestTitle(t *testing.T) {
	for prose, want := range map[string]string{
		"Requirement: Delete guard\nThe system MUST NOT ...": "Delete guard",
		"":                       "x/1",
		strings.Repeat("a", 120): strings.Repeat("a", 87) + "...",
	} {
		if got := Title(prose, "x/1"); got != want {
			t.Errorf("Title(%q) = %q, want %q", prose, got, want)
		}
	}
}
