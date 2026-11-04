package checker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

// dayCount states a computed value exactly: the whole days a loan is
// overdue, between two bounds, and a message that names the loan's tool.
const dayCount = `
context: c
formatVersion: 1
---
entity: Loan
context: c
attributes:
  tool: { type: string }
  lentAt: { type: time }
  daysOverdue: { type: int }
---
extend: platform.Result
context: c
attributes:
  message: { type: string }
---
operation: Loan.overdue
context: c
default: allow
writes: [Loan.daysOverdue]
---
id: D
kind: invariant
on: Loan.overdue
when: now > self.lentAt + 14d
assert: self.lentAt + 14d + days(self.daysOverdue) <= now and now < self.lentAt + 14d + days(self.daysOverdue + 1)
---
id: M
kind: invariant
on: Loan.overdue
when: now > self.lentAt + 14d
assert: contains(result.message, self.tool) and contains(result.message, "overdue")
`

// TestComputedValueDecides: a step that must set a whole number between two
// bounds, and a message that must name its subject, are decided. The answer
// differs in every situation, so refinement over constant responses never
// converged and the quantified query timed out (ISSUES 80); the floor of
// the bound, and the strings joined, answer every situation at once.
func TestComputedValueDecides(t *testing.T) {
	needZ3(t)
	start := time.Now()
	res := check(t, writeRegistry(t, dayCount), lower.Options{})
	if res.Verdict != Pass || len(res.Undecided) > 0 {
		t.Fatalf("got %s, undecided %v, conflicts %q; want pass", res.Verdict, res.Undecided, cores(res))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
	// The candidates only shape the responses tried: a rule no count
	// can meet with D is still a conflict.
	clash := dayCount + "---\nid: Z\nkind: invariant\non: Loan.overdue\nwhen: now > self.lentAt + 20d\nassert: self.daysOverdue < 3\n"
	if res := check(t, writeRegistry(t, clash), lower.Options{}); !slices.Contains(cores(res), "overdue: D + Z") {
		t.Errorf("a count below the bound passed: %s %q, undecided %v", res.Verdict, cores(res), res.Undecided)
	}
	// Nor can the message both name the tool and leave it out. (Every
	// message contains an empty name, so S alone needs one.)
	silent := dayCount + "---\nid: S\nkind: invariant\non: Loan.overdue\nwhen: self.tool != \"\"\nassert: not contains(result.message, self.tool)\n"
	if res := check(t, writeRegistry(t, silent), lower.Options{}); !slices.Contains(cores(res), "overdue: M + S") {
		t.Errorf("a message that must and must not name the tool passed: %s %q, undecided %v", res.Verdict, cores(res), res.Undecided)
	}
}

// perItem lists a shed's tools with a result per tool, keyed by the tool's
// id, which a uniqueness rule makes a key.
const perItem = `
context: c
formatVersion: 1
---
entity: Shed
context: c
relations:
  tools: { to: Tool, many: true }
---
entity: Tool
context: c
attributes:
  id: { type: int }
  status: { type: enum, values: [available, lent] }
  lentAt: { type: time, optional: true }
---
extend: platform.Result
context: c
attributes:
  dueDateOf: { type: "map<int, time>" }
  overdueDaysOf: { type: "map<int, int>" }
  listedIds: { type: list, of: int }
---
operation: Shed.list
context: c
default: allow
---
operation: Shed.overdue
context: c
default: allow
---
id: U
kind: invariant
subject: Tool
assert: "all(u in instances(Tool): u.id != self.id)"
---
id: L
kind: invariant
on: Shed.list
assert: "all(t in self.tools: contains(result.listedIds, t.id))"
---
id: D
kind: invariant
on: Shed.list
assert: "all(t in self.tools: t.status = lent and isSet(t.lentAt) implies result.dueDateOf[t.id] = t.lentAt + 14d)"
---
id: O
kind: invariant
on: Shed.overdue
assert: "all(t in self.tools: t.status = lent and isSet(t.lentAt) and now > t.lentAt + 14d implies days(result.overdueDaysOf[t.id]) <= now - (t.lentAt + 14d) and now - (t.lentAt + 14d) < days(result.overdueDaysOf[t.id] + 1))"
`

// TestPerItemResultsDecide: results keyed by an item's id, over the items
// of a relation to many, are decided. Two items sharing an id were shown
// contradicting a result per id though the uniqueness rule makes the id a
// key (a developer trial, 2026-11-03); then refinement never converged on
// a map per item or a list of every item.
func TestPerItemResultsDecide(t *testing.T) {
	needZ3(t)
	start := time.Now()
	res := check(t, writeRegistry(t, perItem), lower.Options{})
	if res.Verdict != Pass || len(res.Undecided) > 0 {
		t.Fatalf("got %s, undecided %v, conflicts %q; want pass", res.Verdict, res.Undecided, cores(res))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
	// Without the key, two tools with one id and different lent dates
	// have no single due date: a real contradiction.
	noKey := strings.Replace(perItem, "assert: \"all(u in instances(Tool): u.id != self.id)\"", "assert: self.id >= 0", 1)
	if res := check(t, writeRegistry(t, noKey), lower.Options{}); !slices.Contains(cores(res), "list: D") {
		t.Errorf("a due date per shared id passed without the key: %s %q, undecided %v", res.Verdict, cores(res), res.Undecided)
	}
}
