package checker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// checkBoth checks dir with refinement and with the quantified query alone.
func checkBoth(t *testing.T, dir string, opt lower.Options) (refined, quantified *Result) {
	t.Helper()
	refined = check(t, dir, opt)
	refineSituations = false
	defer func() { refineSituations = true }()
	quantified = check(t, dir, opt)
	return refined, quantified
}

// TestRefinementAgrees: refinement and the quantified query find the same
// conflicts, dead responses and vacuous rules.
func TestRefinementAgrees(t *testing.T) {
	dir := filepath.Join("..", "testdata", "project-a", "registry")
	for _, opt := range []lower.Options{{IgnoreDecisions: true}, {}} {
		r, q := checkBoth(t, dir, opt)
		if !reflect.DeepEqual(cores(r), cores(q)) || r.Verdict != q.Verdict || len(r.Dead) != len(q.Dead) || len(r.Vacuous) != len(q.Vacuous) {
			t.Errorf("%+v: refined %s %q, quantified %s %q", opt, r.Verdict, cores(r), q.Verdict, cores(q))
		}
	}
	r, q := checkBoth(t, writeRegistry(t, resources), lower.Options{})
	if !reflect.DeepEqual(cores(r), cores(q)) {
		t.Errorf("resources: refined %q, quantified %q", cores(r), cores(q))
	}
}

// TestManyObligations: an operation with many independent obligations has
// no conflict. Quantifier elimination over its response gives up or takes
// seconds (HANDOFF-performance, 2026-11-01); refinement decides it at once.
func TestManyObligations(t *testing.T) {
	var b strings.Builder
	b.WriteString(`
context: orders
formatVersion: 1
---
extend: platform.Request
context: orders
attributes:
  signed: { type: bool }
---
entity: Order
context: orders
attributes:
  mode: { type: enum, values: [full, partial] }
`)
	const n = 14
	for i := range n {
		fmt.Fprintf(&b, "  checked%d: { type: bool }\n", i)
	}
	b.WriteString("---\noperation: Order.review\ncontext: orders\ndefault: allow\nwrites: [")
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "Order.checked%d", i)
	}
	b.WriteString("]\n---\nid: R0\nkind: permission\neffect: deny\noperation: Order.review\nwhen: not request.signed\n")
	for i := range n {
		fmt.Fprintf(&b, "---\nid: R%d\nkind: invariant\non: Order.review\n", i+1)
		if i == n-1 {
			b.WriteString("when: self.mode = partial\n")
		}
		fmt.Fprintf(&b, "assert: self.checked%d\n", i)
	}
	start := time.Now()
	res := check(t, writeRegistry(t, b.String()), lower.Options{})
	if res.Verdict != Pass || len(res.Undecided) > 0 {
		t.Fatalf("got %s, undecided %v, conflicts %q; want pass", res.Verdict, res.Undecided, cores(res))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

// TestModelCutOff: a solver that answers sat but runs out of time before
// giving the model has not shown a situation. The check is unknown, not an
// internal error (HANDOFF-performance, 2026-11-01).
func TestModelCutOff(t *testing.T) {
	needZ3(t)
	fake := filepath.Join(t.TempDir(), "z3")
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'Z3 version 0 - fake'; exit 0; fi\ncat >/dev/null\necho sat\necho timeout\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(writeRegistry(t, resources))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), &solver.Z3{Path: fake}, prog)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != Unknown {
		t.Errorf("verdict %s, want unknown", res.Verdict)
	}
}

// TestNearLimit: a unit decided only by answers close to the timeout is
// reported, since a slower machine would leave it undecided.
func TestNearLimit(t *testing.T) {
	needZ3(t)
	fake := filepath.Join(t.TempDir(), "z3")
	// Situation searches ask for a model: they answer unsat after 0.85 s of
	// the 1 s timeout. Every other query answers sat at once.
	script := `#!/bin/sh
if [ "$1" = --version ]; then echo 'Z3 version 0 - fake'; exit 0; fi
if grep -q produce-models; then sleep 0.85; echo unsat; else echo sat; fi
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(writeRegistry(t, resources))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), &solver.Z3{Path: fake, Timeout: time.Second}, prog)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.NearLimit, []string{"Resource.delete"}) {
		t.Errorf("near limit %v, want [Resource.delete]", res.NearLimit)
	}
}
