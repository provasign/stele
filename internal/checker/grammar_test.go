package checker

import (
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
)

const outputs = `
context: orders
formatVersion: 1
---
extend: platform.Result
context: orders
attributes:
  notified: { type: bool }
  attempts: { type: int, range: [0, 10] }
  deletedCount: { type: int }
---
entity: Order
context: orders
attributes:
  status: { type: enum, values: [open, cancelled, shipped] }
---
operation: Order.cancel
context: orders
default: allow
writes: [Order.status]
---
operation: Order.ship
context: orders
default: allow
writes: [Order.status]
---
id: N1
kind: invariant
on: Order.cancel
assert: result.notified
---
id: N2
kind: invariant
on: Order.cancel
assert: not result.notified
---
id: A1
kind: invariant
on: Order.ship
assert: result.attempts <= 3
---
id: A2
kind: invariant
on: Order.ship
assert: result.attempts >= 5
`

// TestResultOutputs: what a step produces (result.x) is checked like any
// response: two rules that demand different outputs of one step conflict.
func TestResultOutputs(t *testing.T) {
	res := check(t, writeRegistry(t, outputs), lower.Options{})
	got := strings.Join(cores(res), "; ")
	for _, want := range []string{"cancel: N1 + N2", "ship: A1 + A2"} {
		if !strings.Contains(got, want) {
			t.Errorf("conflicts %q, want %q", got, want)
		}
	}
	consistent := strings.NewReplacer("assert: not result.notified", "assert: result.notified", "assert: result.attempts >= 5", "assert: result.attempts >= 2").Replace(outputs)
	if res := check(t, writeRegistry(t, consistent), lower.Options{}); res.Verdict != Pass {
		t.Errorf("consistent outputs: %s %q", res.Verdict, cores(res))
	}
}

// TestResultOnlyOnSteps: a result exists only after a step; reading it in
// a state invariant, a guard or old() is a type error.
func TestResultOnlyOnSteps(t *testing.T) {
	for _, rule := range []string{
		"id: S1\nkind: invariant\nassert: result.notified\nsubject: Order\n",
		"id: S2\nkind: permission\neffect: deny\noperation: Order.cancel\nwhen: result.notified\n",
		"id: S3\nkind: invariant\non: Order.cancel\nassert: old(result.notified)\n",
	} {
		reg, err := ir.LoadDir(writeRegistry(t, outputs+"---\n"+rule))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lower.Lower(reg, lower.Options{}); err == nil || !strings.Contains(err.Error(), "is what a step produces") {
			t.Errorf("%s: got %v, want a type error", strings.SplitN(rule, "\n", 2)[0], err)
		}
	}
}

const accounts = `
context: billing
formatVersion: 1
---
entity: Account
context: billing
attributes:
  number: { type: int, range: [1, 1000] }
  active: { type: bool }
  balance: { type: decimal, range: [0, 100000] }
---
entity: Config
context: billing
attributes:
  sweepSeconds: { type: int, range: [1, 3600] }
  maxActive: { type: int, range: [0, 100] }
---
operation: Account.open
context: billing
default: allow
writes: [Account.number, Account.active, Account.balance]
---
id: U1
kind: invariant
subject: Account
assert: "all(a in instances(Account): a.number != self.number)"
---
id: C1
kind: invariant
assert: "count(a in instances(Account): a.active) <= config.maxActive"
---
id: T1
kind: invariant
on: Account.open
assert: self.balance = max(old(self.balance) - 10, 0) and abs(self.balance - old(self.balance)) <= 10 and min(self.balance, 100000) = self.balance
---
id: T2
kind: precondition
operation: Account.open
when: seconds(config.sweepSeconds) <= 1h
`

// TestInstancesAndArithmetic: rules range over every other instance of an
// entity with instances(E); seconds() turns a whole number into a duration;
// min, max and abs are exact. All of it lowers, and the check decides it.
func TestInstancesAndArithmetic(t *testing.T) {
	res := check(t, writeRegistry(t, accounts), lower.Options{})
	if res.Verdict == Unknown {
		t.Fatalf("undecided: %v", res.Undecided)
	}
	reg, _ := ir.LoadDir(writeRegistry(t, accounts))
	if r := reg.Entities["Account"].Relations[ir.InstancesRelation]; r == nil || !r.Many {
		t.Fatalf("no instances relation on Account: %+v", r)
	}
	for _, x := range reg.Rules {
		if x.ID == "U1" && !strings.Contains(x.Assert.String(), "instances(Account)") {
			t.Errorf("U1 prints as %s", x.Assert)
		}
	}
	// Uniqueness meets a rule that makes every account number 7: it holds
	// only while no other account exists, which the check allows.
	clash := accounts + "---\nid: U2\nkind: invariant\nassert: \"all(a in instances(Account): a.number = 7) and account.number = 7\"\n"
	if res := check(t, writeRegistry(t, clash), lower.Options{}); res.Verdict == Unknown {
		t.Errorf("undecided: %v", res.Undecided)
	}
}

// TestInstancesAndSecondsDecide: instances() and seconds() carry meaning the
// check uses: uniqueness against another account that shares the number
// conflicts, and an interval that must exceed what its range allows fails.
func TestInstancesAndSecondsDecide(t *testing.T) {
	// Uniqueness meets "every account number is 7": once another account
	// exists (the world's choice), no numbering satisfies both.
	clash := accounts + "---\nid: U2\nkind: invariant\nassert: \"all(a in instances(Account): a.number = 7) and account.number = 7\"\n"
	res := check(t, writeRegistry(t, clash), lower.Options{})
	if got := strings.Join(cores(res), "; "); !strings.Contains(got, "U1 + U2") {
		t.Errorf("uniqueness against one shared number: %s %q", res.Verdict, got)
	}
	if res := check(t, writeRegistry(t, accounts), lower.Options{}); res.Verdict != Pass {
		t.Errorf("the accounts registry alone: %s %q", res.Verdict, cores(res))
	}
	long := accounts + "---\nid: T3\nkind: invariant\nassert: seconds(config.sweepSeconds) > 2h\n"
	if res := check(t, writeRegistry(t, long), lower.Options{}); res.Verdict != Fail {
		t.Errorf("an interval beyond its range passed: %s %q", res.Verdict, cores(res))
	}
}
