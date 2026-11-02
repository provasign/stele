package adopt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

func TestPreviewShowsConflictsARetirementExposes(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir := t.TempDir()
	src := `context: res
formatVersion: 1
---
entity: Resource
context: res
attributes:
  holds: { type: int, range: [0, 5] }
---
operation: Resource.delete
context: res
default: allow
writes: []
---
id: no-holds
kind: invariant
assert: self.holds = 0
subject: Resource
---
id: guard
kind: permission
effect: deny
operation: Resource.delete
when: self.holds > 0
---
id: always
kind: permission
effect: allow
operation: Resource.delete
`
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{IgnoreDecisions: true})
	if err != nil {
		t.Fatal(err)
	}
	z := &solver.Z3{}
	base, err := checker.Check(context.Background(), z, prog)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Conflicts) != 0 {
		t.Fatalf("base conflicts = %+v", base.Conflicts)
	}
	items := []Item{{ID: "T-1", Retire: []string{"no-holds@1"}}, {ID: "T-2", Retire: []string{"guard@1"}}}
	if err := Preview(context.Background(), z, reg, base, items); err != nil {
		t.Fatal(err)
	}
	if len(items[0].Exposes) != 1 || !strings.Contains(items[0].Exposes[0], "always") || !strings.Contains(items[0].Exposes[0], "guard") {
		t.Errorf("retiring no-holds exposes %v, want the guard/always clash", items[0].Exposes)
	}
	if len(items[1].Exposes) != 0 {
		t.Errorf("retiring guard exposes %v, want nothing", items[1].Exposes)
	}
	if reg.Rule("no-holds").Status != "active" {
		t.Error("Preview left a rule retired")
	}
}
