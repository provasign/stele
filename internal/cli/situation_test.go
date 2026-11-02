package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
)

func TestSituationReadsAsValues(t *testing.T) {
	dir := t.TempDir()
	src := `context: c
formatVersion: 1
---
entity: Theme
context: c
attributes:
  tags: { type: set, of: string }
  roles: { type: list, of: string }
  parent: { type: string, optional: true }
  name: { type: string }
`
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := situation(reg, []checker.Binding{
		{Name: "Theme.name", Value: `"default"`},
		{Name: "Theme.parent", Value: `"x"`}, {Name: "Theme.parent?", Value: "false"},
		{Name: "Theme.roles#0", Value: `"a"`}, {Name: "Theme.roles#1", Value: `"!3!"`}, {Name: "Theme.roles#2", Value: `"z"`}, {Name: "Theme.roles#n", Value: "2"},
		{Name: "Theme.tags#0", Value: `"dark"`}, {Name: "Theme.tags#1", Value: `"dark"`}, {Name: "Theme.tags#n", Value: "2"},
	})
	want := []string{`Theme.name="default"`, "Theme.parent unset", `Theme.roles=["a", <other text 3>]`, `Theme.tags={"dark"}`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}
