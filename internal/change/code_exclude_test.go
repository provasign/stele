package change

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestCodeSearchLeavesOutSpecsAndStele: the code search never reads the
// spec folder (Markdown sources are relative to it) or Stele's own files,
// so a spec or a PR card is never "what the code follows".
func TestCodeSearchLeavesOutSpecsAndStele(t *testing.T) {
	repo := t.TempDir()
	opt := Options{Repo: repo, SpecsDir: filepath.Join(repo, "specs"), Registry: filepath.Join(repo, ".stele", "registry"), Out: filepath.Join(repo, ".stele", "change", "c1")}
	got := codeExcludes(opt, []string{"lending.md#L6"})
	for _, want := range []string{"specs", ".stele", "stele.yaml"} {
		if !slices.Contains(got, want) {
			t.Errorf("%q not left out: %v", want, got)
		}
	}
	// A spec root at the repository's root leaves the code in.
	opt.SpecsDir = repo
	if slices.Contains(codeExcludes(opt, nil), ".") {
		t.Errorf("the repository itself left out")
	}
}
