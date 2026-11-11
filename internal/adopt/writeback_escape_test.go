package adopt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/translate"
)

// TestWriteBackStaysInTheRepository: a statement whose source climbs out of
// the repository, directly or through a symlink, gets no note, and the file
// it names is left as it was (a security review, 2026-11-11).
func TestWriteBackStaysInTheRepository(t *testing.T) {
	top := t.TempDir()
	repo := filepath.Join(top, "repo")
	os.MkdirAll(repo, 0o755)
	outside := filepath.Join(top, "target.md")
	os.WriteFile(outside, []byte("A line\n"), 0o644)
	os.Symlink(outside, filepath.Join(repo, "link.md"))
	for _, src := range []string{"../target.md#L1", "link.md#L1", outside + "#L1"} {
		stmts := []translate.Statement{{ID: "a", Source: src, Prose: "x"}, {ID: "b", Source: src, Prose: "y"}}
		items := []Item{{ID: "T1", Decision: Accept, SupersededBy: map[string]string{"a": "b"}, Situation: "s"}}
		if _, err := WriteBack(repo, items, stmts, false); err == nil || !strings.Contains(err.Error(), "repository") {
			t.Errorf("%s: err = %v", src, err)
		}
		if b, _ := os.ReadFile(outside); string(b) != "A line\n" {
			t.Fatalf("%s: the file outside was changed:\n%s", src, b)
		}
	}
}
