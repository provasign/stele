package lint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/provasign/stele/internal/ir"
)

func TestInputsOnlyAndSelfConflicts(t *testing.T) {
	dir := t.TempDir()
	src := `context: c
formatVersion: 1
---
entity: Pager
context: c
attributes:
  exited: { type: bool }
  graceful: { type: bool }
  elapsed: { type: duration }
---
operation: Pager.handleExit
context: c
default: allow
writes: [Pager.graceful]
---
id: A
kind: invariant
on: Pager.handleExit
when: self.exited
assert: self.graceful and self.elapsed <= 1s
---
id: B
kind: invariant
on: Pager.handleExit
assert: self.graceful
`
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := Rules(reg)
	if len(got) != 1 || got[0].Rule != "A" || got[0].Kind != InputsOnly {
		t.Fatalf("findings %+v", got)
	}
	if s := SelfConflicts(reg, [][]string{{"A"}, {"A", "B"}}); len(s) != 1 || s[0] != 0 {
		t.Fatalf("self conflicts %v", s)
	}
}
