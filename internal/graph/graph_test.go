package graph

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/provasign/stele/internal/ir"
)

func load(t *testing.T, src string) *ir.Registry {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestWords(t *testing.T) {
	got := Words("Pager.followEnabled MUST exit with status 141 when pipes break")
	want := []string{"pager", "follow", "enabled", "exit", "status", "pipe", "break"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

// A new statement about a pager's exit code retrieves the exit attributes,
// not the theme's; the subset keeps their entity and the operation writing
// them, and no other entity.
func TestRelatedAndSubset(t *testing.T) {
	reg := load(t, `context: c
formatVersion: 1
---
entity: Pager
context: c
attributes:
  exitCode: { type: int, range: [0, 255], meaning: the status the application exits with }
  brokenPipe: { type: bool }
---
entity: Theme
context: c
attributes:
  name: { type: string }
  version: { type: string }
---
operation: Pager.close
context: c
default: allow
writes: [Pager.exitCode]
---
id: R1
kind: invariant
on: Pager.close
when: self.brokenPipe
assert: self.exitCode = 141
prose: "If the pipe to the pager breaks during output, exit with status 141."
`)
	g := Build(reg)
	rel := g.Related("The application MUST exit with status 0 when the pager exits early", 2)
	var names []string
	for _, a := range rel {
		names = append(names, a.QName())
	}
	if len(names) == 0 || names[0] != "Pager.exitCode" {
		t.Fatalf("related %v", names)
	}
	sub := g.Subset(rel)
	if sub.Entities["Theme"] != nil || sub.Entities["Pager"] == nil || sub.Operations["Pager.close"] == nil || len(sub.Rules) != 0 {
		t.Fatalf("subset %+v", sub)
	}
	if g.Readers[reg.Entities["Pager"].Attrs["exitCode"]][0].ID != "R1" {
		t.Error("readers")
	}
}

func TestGroups(t *testing.T) {
	got := Groups([][]string{{"A", "B"}, {"A", "C"}, {"D", "E"}, {"B", "F"}})
	if len(got) != 3 || got[0].Root != "A" || !reflect.DeepEqual(got[0].Others, []string{"B", "C"}) || got[1].Root != "" && len(got[1].Conflicts) != 1 {
		t.Errorf("groups %+v", got)
	}
}
