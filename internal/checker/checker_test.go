package checker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

func needZ3(t *testing.T) *solver.Z3 {
	t.Helper()
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	return &solver.Z3{}
}

func check(t *testing.T, dir string, opt lower.Options) *Result {
	t.Helper()
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, opt)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), needZ3(t), prog)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// cores names each conflict as "<operation>: <rules>", sorted.
func cores(res *Result) []string {
	var out []string
	for _, c := range res.Conflicts {
		unit := c.Unit
		if i := strings.LastIndexByte(unit, '.'); i >= 0 {
			unit = unit[i+1:]
		}
		out = append(out, unit+": "+strings.Join(c.Rules, " + "))
	}
	sort.Strings(out)
	return out
}

// TestProjectA is suite T-A's first regression test: the Project A
// fixture (an invented service with the contradictions the Project A case
// study found), checked from its registry YAML as a transition system (D-042).
func TestProjectA(t *testing.T) {
	dir := filepath.Join("..", "testdata", "project-a", "registry")

	asWritten := check(t, dir, lower.Options{IgnoreDecisions: true})
	want := []string{
		"stop: AX1 + PA-003-FR025 + PA-014-FR009a",           // C4
		"stop: AX2 + PA-003-FR025 + PA-014-FR009",            // found by stele check
		"ageCheck: AX4 + PA-021-DM-MAXLIFE + PA-021-FR015",   // C5
		"idleSweep: PA-003-FR023 + PA-014-FR009c",            // C2
		"idleSweep: PA-004-FR020 + PA-014-FR009d",            // C3
		"handleAgentCompletion: PA-021-FR014 + PA-021-FR015", // found by stele check
		"destroy: PA-021-FR015 + Workspace.destroy.effect",   // new: deleting a failed task's workspace
		"sendFollowUp: PA-022-FR026 + PA-022-FR028",          // C1, from a state FR-014 makes reachable
	}
	sort.Strings(want)
	if got := cores(asWritten); !reflect.DeepEqual(got, want) {
		t.Errorf("rules as written:\n got %q\nwant %q", got, want)
	}

	// The case study's decisions resolve C2-C5. C1 remains: one-step checking
	// assumes any state the invariants allow, and none of the decisions
	// touches FR-026 or FR-028.
	withDecisions := check(t, dir, lower.Options{})
	want = []string{
		"handleAgentCompletion: PA-021-FR014 + PA-021-FR015",
		"destroy: PA-021-FR015 + Workspace.destroy.effect",
		"sendFollowUp: PA-022-FR026 + PA-022-FR028",
	}
	sort.Strings(want)
	if got := cores(withDecisions); !reflect.DeepEqual(got, want) {
		t.Errorf("with decisions:\n got %q\nwant %q", got, want)
	}
}

func writeRegistry(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const resources = `
context: resources
formatVersion: 1
---
extend: platform.Request
context: resources
attributes:
  kind: { type: enum, values: [normal, erasure] }
---
entity: Resource
context: resources
attributes:
  activeHolds: { type: int, range: [0, 1000] }
  status: { type: enum, values: [active, deleted] }
---
operation: Resource.delete
context: resources
default: deny
writes: [Resource.status]
---
id: R12
version: 3
kind: permission
effect: deny
operation: Resource.delete
when: self.activeHolds > 0
---
id: R31
kind: permission
effect: allow
operation: Resource.delete
when: request.kind = erasure
`

// TestPermissionConflict is the running example of docs/05: R12 denies what
// R31 allows when an erasure request meets a resource with holds; D7 resolves it.
func TestPermissionConflict(t *testing.T) {
	res := check(t, writeRegistry(t, resources), lower.Options{})
	if res.Verdict != Fail || len(res.Conflicts) != 1 {
		t.Fatalf("got %+v, want one conflict", res)
	}
	c := res.Conflicts[0]
	if got := strings.Join(c.Rules, ","); got != "R12,R31" {
		t.Errorf("core = %s, want R12,R31", got)
	}
	cx := map[string]string{}
	for _, b := range c.Counterexample {
		cx[b.Name] = b.Value
	}
	if cx["Request.kind"] != "erasure" || cx["Resource.activeHolds"] == "0" {
		t.Errorf("counterexample %v does not show an erasure with holds", cx)
	}

	resolved := resources + `---
id: D7
resolves: [R31@1, R12@3]
supersede: { winner: R31, loser: R12, when: request.kind = erasure }
`
	if res := check(t, writeRegistry(t, resolved), lower.Options{}); res.Verdict != Pass {
		t.Errorf("with D7: got %+v, want pass", res)
	}
}

func TestStaleDecision(t *testing.T) {
	stale := resources + `---
id: D7
resolves: [R31@1, R12@2]
supersede: { winner: R31, loser: R12, when: request.kind = erasure }
`
	_, err := ir.LoadDir(writeRegistry(t, stale))
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("err = %v, want a stale-decision error", err)
	}
}

func TestTypeErrors(t *testing.T) {
	bad := resources + `---
id: R40
kind: invariant
on: Resource.delete
when: request.kind = archived
assert: self.status = deleted
`
	reg, err := ir.LoadDir(writeRegistry(t, bad))
	if err != nil {
		t.Fatal(err)
	}
	_, err = lower.Lower(reg, lower.Options{})
	if err == nil || !strings.Contains(err.Error(), `"archived" is not a value of Request.kind`) {
		t.Errorf("err = %v, want an unknown-enum-value error", err)
	}
}

// A state no situation allows is reported even though every situation has a
// response: the system escapes the rules by never deleting anything.
func TestDeadResponse(t *testing.T) {
	dead := resources + `---
id: R50
kind: invariant
subject: Resource
when: self.status = deleted
assert: self.activeHolds = 0 and self.activeHolds > 0
`
	res := check(t, writeRegistry(t, dead), lower.Options{IgnoreDecisions: true})
	if len(res.Dead) != 1 || res.Dead[0].Response != "Resource.status = deleted" || strings.Join(res.Dead[0].Rules, ",") != "R50" {
		t.Errorf("dead = %+v, want Resource.status = deleted killed by R50", res.Dead)
	}
}

// A default-deny operation no rule allows never runs. It is reported as a
// vocabulary choice; one that some allow rule names is a dead response.
func TestNeverAllowed(t *testing.T) {
	res := check(t, writeRegistry(t, resources+`---
operation: Resource.archive
context: resources
default: deny
writes: [Resource.status]
`), lower.Options{IgnoreDecisions: true})
	var got []string
	for _, d := range res.Dead {
		got = append(got, fmt.Sprintf("%s never_allowed=%v", d.Response, d.NeverAllowed))
	}
	if !reflect.DeepEqual(got, []string{"Resource.archive runs never_allowed=true"}) {
		t.Errorf("dead = %v, want only Resource.archive, never allowed", got)
	}

	res = check(t, writeRegistry(t, resources+`---
id: R60
kind: precondition
operation: Resource.delete
when: self.activeHolds < 0
`), lower.Options{IgnoreDecisions: true})
	for _, d := range res.Dead {
		if d.NeverAllowed {
			t.Errorf("%s marked never allowed, but R31 allows it", d.Response)
		}
	}
}

// A default of an attribute an operation writes is an initial value. It used
// to be asserted as an assumption over a response variable, which made the
// solver script invalid (Project B, a log-filter spec).
func TestDefaultOfWrittenAttribute(t *testing.T) {
	dir := writeRegistry(t, `
context: c
formatVersion: 1
---
entity: LogFilter
context: c
attributes:
  enabled: { type: bool }
---
operation: LogFilter.toggle
context: c
default: allow
writes: [LogFilter.enabled]
---
id: F1
kind: derivation
assert: logFilter.enabled.default = false
---
id: F2
kind: invariant
on: LogFilter.toggle
when: logFilter.enabled = false
assert: logFilter.enabled = true
`)
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(prog.Warnings, "\n"), "F1: LogFilter.enabled is written by an operation") {
		t.Errorf("warnings = %v, want one about F1", prog.Warnings)
	}
	res, err := Check(context.Background(), needZ3(t), prog)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != Pass {
		t.Errorf("got %+v, want pass", res)
	}
}

// Requirements on attributes no operation writes are assumptions about the
// environment; two that cannot both hold must still be reported.
func TestConflictingAssumptions(t *testing.T) {
	res := check(t, writeRegistry(t, `
context: c
formatVersion: 1
---
entity: Workspace
context: c
attributes:
  idleWarningShown: { type: bool }
  minutesUntilShutdown: { type: int, range: [0, 60] }
---
id: A1
kind: invariant
subject: Workspace
when: self.minutesUntilShutdown <= 5
assert: self.idleWarningShown = true
---
id: A2
kind: invariant
subject: Workspace
assert: self.idleWarningShown = false
`), lower.Options{})
	if got := cores(res); !reflect.DeepEqual(got, []string{"assumptions: A1 + A2"}) {
		t.Fatalf("conflicts = %v, want assumptions: A1 + A2", got)
	}
}

// A rule whose condition the assumptions exclude is reported: it hides any
// conflict it would have.
func TestVacuousRule(t *testing.T) {
	res := check(t, writeRegistry(t, `
context: c
formatVersion: 1
---
entity: Workspace
context: c
attributes:
  idleWarningShown: { type: bool }
  minutesUntilShutdown: { type: int, range: [0, 60] }
---
operation: Workspace.warn
context: c
default: allow
writes: [Workspace.idleWarningShown]
---
id: W1
kind: invariant
subject: Workspace
when: self.minutesUntilShutdown <= 5
assert: self.idleWarningShown = true
---
id: W2
kind: invariant
subject: Workspace
assert: self.idleWarningShown = false
---
id: T1
kind: invariant
subject: Workspace
assert: self.minutesUntilShutdown >= 30
`), lower.Options{})
	if len(res.Conflicts) != 0 {
		t.Errorf("conflicts = %v, want none (T1 hides W1 + W2)", cores(res))
	}
	if len(res.Vacuous) != 1 || res.Vacuous[0].Rule != "W1" || strings.Join(res.Vacuous[0].Because, ",") != "T1" {
		t.Fatalf("vacuous = %+v, want W1 because of T1", res.Vacuous)
	}
}

// TestRequirementOutputs is the shape Project A's translation took: two spec
// statements oblige opposite values of an output no operation writes
// ("warn before an idle shutdown", "never show idle warnings"). Together
// they rule out a workspace idle long enough to auto-stop, which is
// reported (A never applies, because of N and W); the step is checked
// again without them, so the conflict between auto-stopping and never
// auto-stopping an idle workspace is still found.
func TestRequirementOutputs(t *testing.T) {
	reg := `
context: c
formatVersion: 1
---
entity: Workspace
context: c
attributes:
  status: { type: enum, values: [running, stopped] }
  idleMinutes: { type: int, range: [0, 100] }
  warningShown: { type: bool }
---
operation: Workspace.stopIdle
context: c
default: deny
writes: [Workspace.status]
---
id: W
kind: invariant
subject: Workspace
when: self.status = running and self.idleMinutes >= 25
assert: self.warningShown
source: { statementId: s-warn }
---
id: N
kind: invariant
subject: Workspace
assert: not self.warningShown
source: { statementId: s-never-warn }
---
id: A
kind: permission
effect: allow
operation: Workspace.stopIdle
when: self.status = running and self.idleMinutes >= 30
source: { statementId: s-auto-stop }
---
id: D
kind: permission
effect: deny
operation: Workspace.stopIdle
source: { statementId: s-never-auto-stop }
`
	res := check(t, writeRegistry(t, reg), lower.Options{})
	if got := strings.Join(cores(res), "; "); !strings.Contains(got, "stopIdle: A + D") {
		t.Errorf("conflicts %q, want the auto-stop conflict A + D", got)
	}
	found := false
	for _, v := range res.Vacuous {
		found = found || (v.Rule == "A" && strings.Join(v.Because, ",") == "N,W")
	}
	if !found {
		t.Errorf("vacuous %+v, want A ruled out by N and W", res.Vacuous)
	}
}

// TestAvoidedRule is Project A's K5 once both statements name one fact:
// "warn when the deadline is near" and "never show a warning". Neither
// step nor state check fails, since the system may simply never be near
// the deadline while running; the warning rule is reported as avoided.
func TestAvoidedRule(t *testing.T) {
	res := check(t, writeRegistry(t, `
context: c
formatVersion: 1
---
entity: Vm
context: c
attributes:
  status: { type: enum, values: [running, stopped] }
  minutesToDeadline: { type: int, range: [0, 60] }
  warningShown: { type: bool }
---
operation: Vm.stop
context: c
default: allow
writes: [Vm.status]
---
id: W
kind: invariant
subject: Vm
when: self.status = running and self.minutesToDeadline <= 5
assert: self.warningShown
source: { statementId: s-warn }
---
id: N
kind: invariant
subject: Vm
assert: not self.warningShown
source: { statementId: s-never-warn }
`), lower.Options{})
	for _, v := range res.Vacuous {
		if v.Rule == "W" && v.Avoided && strings.Join(v.Because, ",") == "N" {
			return
		}
	}
	t.Fatalf("vacuous = %+v, conflicts = %v; want W avoided because of N", res.Vacuous, cores(res))
}

// TestInvariantOverOtherEntityHoldsForOthers: "a member on the waitlist
// holds no plot" ranges over plots; restated for the other members a
// release looks at, the first member waiting holds no plot already, and
// handing them the released plot keeps "a member holds at most one plot".
func TestInvariantOverOtherEntityHoldsForOthers(t *testing.T) {
	src := `context: c
formatVersion: 1
---
entity: Plot
context: c
attributes:
  holder: { type: optional<string> }
---
entity: Member
context: c
attributes:
  email: { type: string }
  waiting: { type: bool }
---
operation: Plot.release
context: c
default: allow
writes: [Plot.holder]
---
id: onePlot
kind: invariant
subject: Plot
when: isSet(self.holder)
assert: 'all(p in instances(Plot): p.holder != self.holder)'
status: active
---
id: waitingHoldsNone
kind: invariant
subject: Member
when: self.waiting
assert: 'all(p in instances(Plot): p.holder != self.email)'
status: active
---
id: toFirstWaiting
kind: invariant
on: Plot.release
when: 'any(m in instances(Member): m.waiting)'
assert: 'any(m in instances(Member): m.waiting and self.holder = m.email)'
status: active
`
	if res := check(t, writeRegistry(t, src), lower.Options{}); res.Verdict != Pass {
		t.Errorf("got %s, want pass: %+v", res.Verdict, res.Conflicts)
	}
}
