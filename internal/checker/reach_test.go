package checker

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

const jobs = `context: c
formatVersion: 1
---
entity: Job
context: c
createdBy: [Job.create]
attributes:
  status: { type: enum, values: [new, running, done, broken] }
  flag: { type: bool }
---
operation: Job.create
context: c
default: allow
writes: [Job.status, Job.flag]
---
operation: Job.start
context: c
default: allow
writes: [Job.status]
---
operation: Job.finish
context: c
default: allow
writes: [Job.status, Job.flag]
---
id: created-new
kind: invariant
on: Job.create
assert: self.status = new
---
id: started-running
kind: invariant
on: Job.start
assert: self.status = running
---
id: finished-done
kind: invariant
on: Job.finish
assert: self.status = done
---
id: broken-flag
kind: invariant
on: Job.finish
when: self.status = broken
assert: self.flag
---
id: broken-no-flag
kind: invariant
on: Job.finish
when: self.status = broken
assert: not self.flag
`

func checkSource(t *testing.T, src string) *Result {
	t.Helper()
	reg, err := ir.LoadDir(writeRegistry(t, src))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), &solver.Z3{}, prog)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestConflictFromUnreachableStateIsSetAside(t *testing.T) {
	needZ3(t)
	res := checkSource(t, jobs)
	if len(res.Conflicts) != 0 || len(res.Unreachable) != 1 || strings.Join(res.Unreachable[0].Rules, ",") != "broken-flag,broken-no-flag" {
		t.Fatalf("conflicts %+v, unreachable %+v", res.Conflicts, res.Unreachable)
	}
	// Without its creating operation a job may start broken.
	res = checkSource(t, strings.Replace(jobs, "createdBy: [Job.create]\n", "", 1))
	if len(res.Conflicts) != 1 || len(res.Unreachable) != 0 {
		t.Fatalf("no createdBy: conflicts %+v, unreachable %+v", res.Conflicts, res.Unreachable)
	}
	// A step that can set broken makes the clash real.
	res = checkSource(t, strings.Replace(jobs, "assert: self.status = running", "assert: self.status in {running, broken}", 1))
	if len(res.Conflicts) != 1 || len(res.Unreachable) != 0 {
		t.Fatalf("start may break: conflicts %+v, unreachable %+v", res.Conflicts, res.Unreachable)
	}
}

// TestClashAlsoInReachableStateStands: the rules clash both when a job is
// broken, which no step enters, and when it is running, which starting
// enters. Set aside on a sample in the broken state, the clash passed (a
// code review, 2026-11-04).
func TestClashAlsoInReachableStateStands(t *testing.T) {
	needZ3(t)
	both := strings.ReplaceAll(jobs, "when: self.status = broken", "when: self.status in {broken, running}")
	res := checkSource(t, both)
	if len(res.Conflicts) != 1 || len(res.Unreachable) != 0 {
		t.Fatalf("conflicts %+v, unreachable %+v", res.Conflicts, res.Unreachable)
	}
	for _, b := range res.Conflicts[0].Situation {
		if strings.HasSuffix(b.Name, "status") && b.Value != "running" {
			t.Errorf("reported in %s %s, want the reachable running", b.Name, b.Value)
		}
	}
}

const idle = `context: c
formatVersion: 1
---
entity: Session
context: c
attributes:
  minutesLeft: { type: int, range: [0, 60], setBy: world }
  warned: { type: bool }
---
operation: Session.activity
context: c
default: allow
writes: [Session.minutesLeft]
effect: { minutesLeft: 60 }
---
operation: Session.idleSweep
context: c
default: allow
writes: [Session.warned]
---
id: warn-near-shutdown
kind: invariant
on: Session.idleSweep
when: self.minutesLeft <= 5
assert: self.warned
---
id: never-warn
kind: invariant
on: Session.idleSweep
assert: not self.warned
`

func TestWorldDrivenValueResetByAStep(t *testing.T) {
	needZ3(t)
	res := checkSource(t, idle)
	if len(res.Conflicts) != 1 || strings.Join(res.Conflicts[0].Rules, ",") != "never-warn,warn-near-shutdown" {
		t.Fatalf("conflicts = %+v", res.Conflicts)
	}
	// Activity resets the timer: a rule wanting another value clashes.
	res = checkSource(t, idle+"---\nid: reset-to-30\nkind: invariant\non: Session.activity\nassert: self.minutesLeft = 30\n")
	if !slices.ContainsFunc(res.Conflicts, func(c Conflict) bool { return slices.Contains(c.Rules, "reset-to-30") }) {
		t.Fatalf("reset clash missing: %+v", res.Conflicts)
	}
	// A step that writes it without a declared reset would choose it.
	_, err := ir.LoadDir(writeRegistry(t, strings.Replace(idle, "writes: [Session.warned]", "writes: [Session.warned, Session.minutesLeft]", 1)))
	if err == nil || !strings.Contains(err.Error(), "may only reset it") {
		t.Fatalf("load error = %v", err)
	}
}

// loans: only lending makes a tool lent, and it sets the lent date; a
// return copies the date into a record that must have one, and a return of
// a tool that is not lent changes nothing. With no step
// known to create tools, a lent tool with no date clashes with that record.
const loans = `
context: c
formatVersion: 1
---
entity: Tool
context: c
attributes:
  status: { type: enum, values: [available, lent] }
  lentAt: { type: time, optional: true }
  returnedLentAt: { type: time }
---
operation: Tool.add
context: c
default: allow
writes: [Tool.status, Tool.lentAt]
---
operation: Tool.lend
context: c
default: allow
writes: [Tool.status, Tool.lentAt]
---
operation: Tool.return
context: c
default: allow
writes: [Tool.status, Tool.lentAt, Tool.returnedLentAt]
---
id: A
kind: invariant
on: Tool.add
assert: self.status = available and self.lentAt = none
---
id: L
kind: invariant
on: Tool.lend
assert: self.status = lent and self.lentAt = now
---
id: R
kind: invariant
on: Tool.return
when: self.status = lent
assert: self.status = available and self.lentAt = none and isSet(old(self.lentAt)) and self.returnedLentAt = old(self.lentAt)
---
id: R2
kind: invariant
on: Tool.return
when: self.status != lent
assert: self.status = old(self.status) and self.lentAt = old(self.lentAt)
`

// TestCreatorsSettingAside: the steps that, recorded as creating tools,
// would set a clash aside are named (any of them would here: none can leave
// a lent tool without a date), and recording one does set it aside.
func TestCreatorsSettingAside(t *testing.T) {
	needZ3(t)
	reg, err := ir.LoadDir(writeRegistry(t, loans))
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), &solver.Z3{}, prog)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Unit != "Tool.return" {
		t.Fatalf("conflicts %q; want the return's", cores(res))
	}
	ops, err := CreatorsSettingAside(context.Background(), &solver.Z3{}, reg, res.Conflicts[0])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ops, "Tool.add") {
		t.Fatalf("creators %v, want Tool.add among them", ops)
	}
	if len(reg.Entities["Tool"].CreatedBy) != 0 {
		t.Fatal("the registry was left changed")
	}
	created := loans + "---\nextend: Tool\ncontext: c\ncreatedBy: [Tool.add]\n"
	if res := checkSource(t, created); res.Verdict != Pass {
		t.Errorf("with Tool.add creating tools: %s %q", res.Verdict, cores(res))
	}
}

// TestWorldFactIsNotOverridden: the lending request came with a fact about
// the world, the lend command's text; the limit that overrides lending
// weakened that fact too, which made it a rule over tool state that a
// return broke by the world choosing another text (a developer trial,
// 2026-11-04).
func TestWorldFactIsNotOverridden(t *testing.T) {
	needZ3(t)
	res := checkSource(t, `context: c
formatVersion: 1
---
entity: Tool
context: c
attributes:
  status: { type: enum, values: [available, lent] }
  holder: { type: optional<string> }
---
entity: Cli
context: c
attributes:
  lendCommand: { type: string, setBy: world }
---
operation: Tool.lend
context: c
default: allow
writes: [Tool.status, Tool.holder]
---
operation: Tool.return
context: c
default: allow
writes: [Tool.status, Tool.holder]
---
id: L
kind: invariant
on: Tool.lend
assert: self.status = lent
---
id: L.2
kind: invariant
assert: cli.lendCommand = "toolshed lend"
---
id: R
kind: invariant
on: Tool.return
assert: self.status = available and self.holder = none
---
id: M
kind: invariant
when: self.status = lent
assert: isSet(self.holder)
overrides: [L]
`)
	if res.Verdict != Pass {
		t.Fatalf("%s %q", res.Verdict, cores(res))
	}
}

// TestRefusedStepIsNotPossible: being able to do something is running with
// an effect. A lend refused by reporting an error and keeping every value
// still ran, so "stewards may have up to 5 tools lent" passed beside a
// 3-tool limit read that way (a developer trial, 2026-11-04). The lent date
// is optional and unset: its leftover value is no effect.
func TestRefusedStepIsNotPossible(t *testing.T) {
	needZ3(t)
	src := `context: c
formatVersion: 1
---
entity: Tool
context: c
attributes:
  status: { type: enum, values: [available, lent] }
  lentAt: { type: optional<time> }
  held: { type: int, range: [0, 9], setBy: world }
---
extend: Result
context: c
attributes:
  error: { type: bool }
---
operation: Tool.lend
context: c
default: allow
writes: [Tool.status, Tool.lentAt]
---
id: limit
kind: invariant
on: Tool.lend
when: self.held >= 3
assert: result.error and self.status = old(self.status) and self.lentAt = old(self.lentAt)
---
id: steward
kind: permission
operation: Tool.lend
effect: possible
when: self.held = 4
`
	if res := checkSource(t, src); res.Verdict != Fail || len(res.Conflicts) != 1 || strings.Join(res.Conflicts[0].Rules, ",") != "limit,steward" {
		t.Fatalf("%s %q", res.Verdict, cores(res))
	}
	// Below the limit the lend has its effect.
	if res := checkSource(t, strings.Replace(src, "when: self.held = 4", "when: self.held = 2", 1)); res.Verdict != Pass {
		t.Fatalf("below the limit: %s %q", res.Verdict, cores(res))
	}
}
