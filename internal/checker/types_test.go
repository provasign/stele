package checker

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/oracle"
	"github.com/provasign/stele/internal/solver"
)

// Each D-050 type, with conflicts known by construction, checked by the SMT
// lowering and by the independent enumerating oracle.
func TestTypesAgainstOracle(t *testing.T) {
	cases := []struct {
		name, reg string
		want      map[string][][]string // unit -> minimal conflicting rule sets
	}{
		{"string prefix, equality and contains", `
entity: Api
context: c
attributes:
  route: { type: string }
---
operation: Api.set
context: c
default: allow
writes: [Api.route]
---
id: R1
kind: invariant
assert: startsWith(api.route, "/api/")
---
id: R2
kind: invariant
assert: api.route = "/health"
---
id: R3
kind: invariant
assert: api.route contains "v2"
`, map[string][][]string{"state": {{"R1", "R2"}, {"R2", "R3"}}}},

		// An optional enum's presence flag is a Bool that carries the
		// enum's attribute; dead-response checks must not pin it to a value.
		{"optional enum in a state rule", `
entity: Tool
context: c
attributes:
  error: { type: enum, values: [riskExceeded], optional: true }
  status: { type: enum, values: [ok, blocked] }
---
operation: Tool.prepare
context: c
default: allow
writes: [Tool.status, Tool.error]
---
id: R1
kind: invariant
assert: tool.error = riskExceeded implies tool.status = blocked
`, map[string][][]string{}},

		{"string request in permissions", `
entity: Api
context: c
attributes:
  served: { type: bool }
---
extend: platform.Request
attributes:
  path: { type: string }
---
operation: Api.serve
context: c
default: deny
writes: [Api.served]
---
id: A
kind: permission
operation: Api.serve
effect: allow
when: startsWith(request.path, "/admin")
---
id: D
kind: permission
operation: Api.serve
effect: deny
when: contains(request.path, "admin")
`, map[string][][]string{"Api.serve": {{"A", "D"}}}},

		{"decimal ranges, mixed with ints", `
entity: Account
context: c
attributes:
  ratio: { type: decimal, range: [0, 1] }
---
operation: Account.rebalance
context: c
default: allow
writes: [Account.ratio]
---
id: R1
kind: invariant
assert: account.ratio >= 0.8
---
id: R2
kind: invariant
assert: account.ratio < 0.5
---
id: R3
kind: invariant
assert: account.ratio <= 1
`, map[string][][]string{"state": {{"R1", "R2"}}}},

		{"durations and times", `
entity: Session
context: c
attributes:
  idle: { type: duration }
  expiresAt: { type: time }
  warned: { type: bool }
  active: { type: bool }
---
operation: Session.tick
context: c
default: allow
writes: [Session.warned, Session.active]
---
id: W1
kind: invariant
when: session.idle >= 15m
assert: session.warned
---
id: W2
kind: invariant
assert: not session.warned
---
id: T1
kind: invariant
when: session.expiresAt < 2025-01-01
assert: not session.active
---
id: T2
kind: invariant
when: session.expiresAt > 2024-12-31
assert: session.active
`, map[string][][]string{"state": {{"T1", "T2"}, {"W1", "W2"}}}},

		{"optional values", `
entity: User
context: c
attributes:
  manager: { type: string, optional: true }
---
operation: User.assign
context: c
default: allow
writes: [User.manager]
---
id: R1
kind: invariant
assert: isSet(user.manager)
---
id: R2
kind: invariant
assert: user.manager = none
---
id: R3
kind: invariant
assert: user.manager = "bob"
`, map[string][][]string{"state": {{"R1", "R2"}, {"R2", "R3"}}}}, // an unset value equals nothing (D-052)

		{"an unset optional equals nothing", `
entity: Pager
context: c
attributes:
  followEnabled: { type: bool, optional: true }
  enabled: { type: bool }
---
operation: Pager.configure
context: c
default: allow
writes: [Pager.enabled]
---
id: R1
kind: invariant
on: Pager.configure
when: self.followEnabled = true
assert: self.enabled
---
id: R2
kind: invariant
on: Pager.configure
when: not isSet(self.followEnabled) or self.followEnabled = false
assert: not self.enabled
---
id: R3
kind: invariant
on: Pager.configure
when: self.followEnabled
assert: not self.enabled
`, map[string][][]string{"Pager.configure": {{"R1", "R3"}}}},

		{"records", `
entity: Job
context: c
attributes:
  result:
    type: record
    fields:
      code: { type: int, range: [0, 2] }
      ok: { type: bool }
---
operation: Job.finish
context: c
default: allow
writes: [Job.result.code, Job.result.ok]
---
id: R1
kind: invariant
on: Job.finish
assert: self.result.ok implies self.result.code = 0
---
id: R2
kind: invariant
on: Job.finish
assert: self.result.ok and self.result.code = 1
`, map[string][][]string{"Job.finish": {{"R1", "R2"}}}},

		{"lists", `
entity: Doc
context: c
attributes:
  tags: { type: list, of: enum, values: [a, b], maxLength: 2 }
---
operation: Doc.tag
context: c
default: allow
writes: [Doc.tags]
---
id: R1
kind: invariant
assert: contains(doc.tags, a)
---
id: R2
kind: invariant
assert: all(t in doc.tags | t = b)
---
id: R3
kind: invariant
assert: len(doc.tags) <= 1
`, map[string][][]string{"state": {{"R1", "R2"}}}},

		{"sets and count", `
entity: Role
context: c
attributes:
  perms: { type: set, of: enum, values: [read, write, admin] }
---
operation: Role.grant
context: c
default: allow
writes: [Role.perms]
---
id: R1
kind: invariant
assert: contains(role.perms, admin) implies contains(role.perms, write)
---
id: R2
kind: invariant
assert: contains(role.perms, admin)
---
id: R3
kind: invariant
assert: not contains(role.perms, write)
---
id: R4
kind: invariant
assert: 'count(p in role.perms: p != read) <= 1'
`, map[string][][]string{"state": {{"R1", "R2", "R3"}, {"R1", "R2", "R4"}}}},

		{"maps", `
entity: Plan
context: c
attributes:
  limits: { type: map, key: enum, keyValues: [gold, silver], of: int, range: [0, 3] }
---
operation: Plan.set
context: c
default: allow
writes: [Plan.limits]
---
id: R1
kind: invariant
assert: has(plan.limits, gold)
---
id: R2
kind: invariant
assert: plan.limits[gold] > 2
---
id: R3
kind: invariant
assert: plan.limits[gold] < 2
`, map[string][][]string{"state": {{"R2", "R3"}}}},

		{"relations to many: stop deletes the VM under a running sibling", `
entity: Node
context: c
attributes:
  vmExists: { type: bool }
relations:
  workspaces: { to: Workspace, many: true, max: 2, inverse: node }
---
entity: Workspace
context: c
attributes:
  status: { type: enum, values: [running, stopped] }
---
operation: Workspace.stop
context: c
default: allow
writes: [Workspace.status, Node.vmExists]
effect: { status: stopped }
---
id: R1
kind: invariant
on: Workspace.stop
assert: not self.node.vmExists
---
id: R2
kind: invariant
assert: all(w in node.workspaces, w.status = running implies node.vmExists)
---
id: R3
kind: invariant
assert: 'all(w in node.workspaces: w.status = running implies w.node.vmExists)'
`, map[string][][]string{"Workspace.stop": {{"R1", "R2"}, {"R1", "R3"}}}},

		{"optional relations", `
entity: Project
context: c
attributes:
  name: { type: string }
---
entity: Workspace
context: c
attributes:
  busy: { type: bool }
relations:
  project: { to: Project, optional: true }
---
operation: Workspace.detach
context: c
default: allow
writes: [Workspace.project]
---
id: R1
kind: invariant
assert: isSet(workspace.project)
---
id: R2
kind: invariant
assert: workspace.project = none
`, map[string][][]string{"state": {{"R1", "R2"}}}},

		{"old values in a step", `
entity: Retry
context: c
attributes:
  delay: { type: int, range: [0, 8] }
---
operation: Retry.again
context: c
default: allow
writes: [Retry.delay]
---
id: R1
kind: invariant
on: Retry.again
when: self.delay <= 4
assert: self.delay = old(self.delay) * 2
---
id: R2
kind: invariant
on: Retry.again
when: self.delay = 4
assert: self.delay < 8
`, map[string][][]string{"Retry.again": {{"R1", "R2"}}}},

		{"history: before, and what a step records", `
entity: Workspace
context: c
attributes:
  up: { type: bool }
---
operation: Workspace.clone
context: c
default: allow
writes: []
---
operation: Workspace.start
context: c
default: allow
writes: [Workspace.up]
---
id: R1
kind: precondition
operation: Workspace.start
when: ran(Workspace.clone)
---
id: R2
kind: permission
operation: Workspace.start
effect: allow
when: not ran(Workspace.clone)
---
id: R3
kind: invariant
on: Workspace.clone
assert: not ran(Workspace.clone)
`, map[string][][]string{"Workspace.clone": {{"R3", "Workspace.clone.history"}}, "Workspace.start": {{"R1", "R2"}}}},

		{"the clock and the last run", `
entity: Job
context: c
attributes:
  ok: { type: bool }
---
operation: Job.sweep
context: c
default: allow
writes: [Job.ok]
---
id: R1
kind: invariant
assert: 'isSet(lastRun(Job.sweep)) implies now - lastRun(Job.sweep) <= 5m'
---
id: R2
kind: invariant
assert: isSet(lastRun(Job.sweep)) and lastRun(Job.sweep) <= now - 1h
`, map[string][][]string{"state": {{"R1", "R2"}}}},

		{"rules about every operation", `
entity: Doc
context: c
attributes:
  gone: { type: bool }
---
extend: platform.Request
attributes:
  authenticated: { type: bool }
---
operation: Doc.edit
context: c
default: allow
writes: []
---
operation: Doc.delete
context: c
default: allow
writes: [Doc.gone]
---
id: R1
kind: precondition
operation: "*"
when: request.authenticated
---
id: R2
kind: permission
operation: Doc.delete
effect: allow
when: not request.authenticated
---
id: R3
kind: precondition
operation: Doc.*
when: request.authenticated
`, map[string][][]string{"Doc.delete": {{"R1", "R2"}, {"R2", "R3"}}}},

		{"regular expressions", `
entity: User
context: c
attributes:
  slug: { type: string }
---
operation: User.rename
context: c
default: allow
writes: [User.slug]
---
id: R1
kind: invariant
assert: matches(user.slug, "[a-z0-9-]+")
---
id: R2
kind: invariant
assert: user.slug = "Bad Slug"
---
id: R3
kind: invariant
assert: user.slug = "good-1"
`, map[string][][]string{"state": {{"R1", "R2"}, {"R2", "R3"}}}},

		{"counts beyond the default bound", `
entity: Theme
context: c
attributes:
  builtin: { type: bool }
relations:
  roles: { to: Role, many: true }
---
entity: Role
context: c
attributes:
  explicit: { type: bool }
---
id: R1
kind: invariant
when: theme.builtin
assert: count(r in theme.roles | r.explicit) = 5
---
id: R2
kind: invariant
when: theme.builtin
assert: count(r in theme.roles | r.explicit) = 4
`, map[string][][]string{"assumptions": {{"R1", "R2"}}}},

		{"support means possible, not always", `
entity: Color
context: c
attributes:
  kind: { type: enum, values: [basic, rgb] }
  parsing: { type: bool }
  valid: { type: bool }
---
operation: Color.validate
context: c
default: allow
writes: [Color.valid]
---
id: S1
kind: permission
operation: Color.validate
effect: possible
when: self.kind = rgb
---
id: P1
kind: precondition
operation: Color.validate
when: self.parsing
---
id: S2
kind: permission
operation: Color.validate
effect: possible
when: self.kind = basic and not self.parsing
`, map[string][][]string{"Color.validate": {{"P1", "S2"}}}},

		{"precedence the text states, and obligations", `
entity: Pager
context: c
attributes:
  color: { type: enum, values: [always, never, auto] }
  follow: { type: bool }
  toFile: { type: bool }
  enabled: { type: bool }
---
operation: Pager.configure
context: c
default: allow
writes: [Pager.enabled]
---
id: A
kind: invariant
on: Pager.configure
when: self.color = always
assert: self.enabled
---
id: B
kind: invariant
on: Pager.configure
when: self.follow
assert: not self.enabled
overrides: [A]
---
id: C
kind: invariant
on: Pager.configure
when: self.toFile
assert: not self.enabled
---
id: T
kind: obligation
prose: pixel regression tests exist
`, map[string][][]string{"Pager.configure": {{"A", "C"}}}},

		{"sets of strings", `
entity: Listing
context: c
attributes:
  tags: { type: set, of: string, maxLength: 2 }
---
operation: Listing.filter
context: c
default: allow
writes: [Listing.tags]
---
id: E
kind: invariant
assert: not isEmpty(listing.tags)
---
id: A
kind: invariant
assert: all(t in listing.tags | startsWith(t, "x"))
---
id: L
kind: invariant
assert: contains(listing.tags, "dark")
---
id: N
kind: invariant
assert: len(listing.tags) <= 1
---
id: S
kind: invariant
assert: listing.tags = ["dark", "xl"]
`, map[string][][]string{"state": {{"A", "L"}, {"A", "S"}, {"N", "S"}}}},

		{"list positions and literals", `
entity: Cmd
context: c
attributes:
  args: { type: list, of: string, maxLength: 2 }
---
operation: Cmd.parse
context: c
default: allow
writes: [Cmd.args]
---
id: N
kind: invariant
assert: cmd.args = ["-R", "-F"]
---
id: P
kind: invariant
assert: cmd.args[0] = "-F"
---
id: Q
kind: invariant
assert: cmd.args[1] = "-F"
`, map[string][][]string{"state": {{"N", "P"}}}},

		{"possible under a deny default", `
entity: Listing
context: c
attributes:
  base: { type: bool }
  shown: { type: bool }
---
operation: Listing.list
context: c
default: deny
writes: [Listing.shown]
---
id: M
kind: permission
operation: Listing.list
effect: possible
when: self.base
---
id: X
kind: permission
operation: Listing.list
effect: deny
when: self.base
`, map[string][][]string{"Listing.list": {{"M", "X"}}}},

		{"list lengths beyond the default bound", `
entity: Theme
context: c
attributes:
  roles: { type: list, of: string }
---
id: R18
kind: invariant
assert: len(theme.roles) = 5
---
id: R16
kind: invariant
assert: len(theme.roles) = 4
`, map[string][][]string{"assumptions": {{"R16", "R18"}}}},

		// x = old(x) keeps x: unset before and after counts as kept, though
		// an unset value equals nothing otherwise (D-052).
		{"an optional value kept by a step", `
entity: Tool
context: c
attributes:
  borrower: { type: optional<int>, range: [0, 2] }
  lent: { type: bool }
---
operation: Tool.lend
context: c
default: allow
writes: [Tool.lent]
---
id: K
kind: invariant
on: Tool.lend
assert: self.borrower = old(self.borrower)
`, map[string][][]string{}},

		// D-064: a step's result is its own response, with no frame and
		// nothing the world chooses. Bounding attempts by 7 alone is met.
		{"step outputs", `
extend: platform.Result
context: c
attributes:
  notified: { type: bool }
  attempts: { type: int, range: [0, 10] }
---
entity: Order
context: c
attributes:
  status: { type: enum, values: [open, cancelled] }
---
operation: Order.cancel
context: c
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
on: Order.cancel
assert: result.attempts <= 3
---
id: A2
kind: invariant
on: Order.cancel
assert: result.attempts >= 5
---
id: A3
kind: invariant
on: Order.cancel
assert: result.attempts <= 7
`, map[string][][]string{"Order.cancel": {{"A1", "A2"}, {"N1", "N2"}}}},

		// D-064: instances(E) ranges over the other instances, whose
		// existence the world decides. Unique numbers fit in [1, 4] with
		// three others; "every number is 2" does not, once another exists.
		{"instances", `
entity: Account
context: c
attributes:
  number: { type: int, range: [1, 4] }
---
operation: Account.open
context: c
default: allow
writes: [Account.number]
---
id: U1
kind: invariant
assert: "all(a in instances(Account): a.number != account.number)"
---
id: U2
kind: invariant
assert: "all(a in instances(Account): a.number = 2) and account.number = 2"
`, map[string][][]string{"state": {{"U1", "U2"}}}},

		// D-064: seconds() and the other conversions. A setting the world
		// gives is an assumption, so it conflicts only with another one.
		{"duration conversions", `
entity: Job
context: c
attributes:
  intervalSeconds: { type: int, range: [1, 10] }
---
id: T1
kind: invariant
assert: seconds(job.intervalSeconds) <= 5s
---
id: T2
kind: invariant
assert: minutes(job.intervalSeconds) >= 60s and hours(job.intervalSeconds) < days(job.intervalSeconds)
---
id: T3
kind: invariant
assert: job.intervalSeconds >= 6
`, map[string][][]string{"assumptions": {{"T1", "T3"}}}},

		// D-064: min, max and abs over a step's change.
		{"min, max and abs over a step's change", `
entity: Wallet
context: c
attributes:
  balance: { type: int, range: [0, 20] }
---
operation: Wallet.charge
context: c
default: allow
writes: [Wallet.balance]
---
id: M1
kind: invariant
on: Wallet.charge
assert: self.balance = max(old(self.balance) - 3, 0)
---
id: M2
kind: invariant
on: Wallet.charge
assert: abs(self.balance - old(self.balance)) <= 3
---
id: M3
kind: invariant
on: Wallet.charge
assert: min(self.balance, 5) = self.balance
`, map[string][][]string{"Wallet.charge": {{"M1", "M3"}, {"M2", "M3"}}}},

		// What must be possible in a situation the invariants rule out
		// conflicts with those invariants, not only with itself.
		{"possible against a state invariant", `
entity: Member
context: c
attributes:
  booksOnLoan: { type: int, range: [0, 10] }
---
operation: Member.borrow
context: c
default: allow
writes: [Member.booksOnLoan]
---
id: F1
kind: invariant
assert: member.booksOnLoan <= 5
---
id: P1
kind: permission
operation: Member.borrow
effect: possible
when: self.booksOnLoan = 9
`, map[string][][]string{"Member.borrow": {{"F1", "P1"}}}},

		// ISSUES 71: contradicting assumptions about something a step does
		// not touch are the assumptions unit's finding; they do not make the
		// step vacuous and hide its own conflict.
		{"unrelated contradicting assumptions", `
entity: Job
context: c
attributes:
  intervalSeconds: { type: int, range: [1, 10] }
---
entity: Wallet
context: c
attributes:
  balance: { type: int, range: [0, 20] }
---
operation: Wallet.charge
context: c
default: allow
writes: [Wallet.balance]
---
id: T1
kind: invariant
assert: job.intervalSeconds <= 5
---
id: T3
kind: invariant
assert: job.intervalSeconds >= 6
---
id: M1
kind: invariant
on: Wallet.charge
assert: self.balance = max(old(self.balance) - 3, 0)
---
id: M3
kind: invariant
on: Wallet.charge
assert: self.balance <= 5
`, map[string][][]string{"assumptions": {{"T1", "T3"}}, "Wallet.charge": {{"M1", "M3"}}}},

		// Contradicting state invariants about what another operation writes
		// are the state unit's finding; the step's question leaves them out.
		{"unrelated contradicting state invariants", `
entity: Job
context: c
attributes:
  armed: { type: bool }
---
operation: Job.arm
context: c
default: allow
writes: [Job.armed]
---
entity: Wallet
context: c
attributes:
  balance: { type: int, range: [0, 20] }
---
operation: Wallet.charge
context: c
default: allow
writes: [Wallet.balance]
---
id: S1
kind: invariant
assert: job.armed
---
id: S2
kind: invariant
assert: not job.armed
---
id: M1
kind: invariant
on: Wallet.charge
assert: self.balance = max(old(self.balance) - 3, 0)
---
id: M3
kind: invariant
on: Wallet.charge
assert: self.balance <= 5
`, map[string][][]string{"state": {{"S1", "S2"}}, "Wallet.charge": {{"M1", "M3"}}}},

		// The same holds for contradicting assumptions about what the step
		// reads: reported once, then left out of every other unit, so the
		// step's own conflict (a world-chosen limit below 15) still shows.
		{"connected contradicting assumptions", `
entity: Wallet
context: c
attributes:
  balance: { type: int, range: [0, 20] }
  limit: { type: int, range: [0, 20] }
---
operation: Wallet.charge
context: c
default: allow
writes: [Wallet.balance]
---
id: L1
kind: invariant
assert: wallet.limit <= 5
---
id: L2
kind: invariant
assert: wallet.limit >= 10
---
id: M1
kind: invariant
on: Wallet.charge
assert: self.balance = max(old(self.balance) - 3, 0)
---
id: M5
kind: invariant
on: Wallet.charge
assert: self.balance <= self.limit and self.balance >= 15
`, map[string][][]string{"assumptions": {{"L1", "L2"}}, "Wallet.charge": {{"M5"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeRegistry(t, "context: c\nformatVersion: 1\n---\n"+c.reg)
			reg, err := ir.LoadDir(dir)
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
			got := map[string][][]string{}
			for _, cf := range res.Conflicts {
				got[cf.Unit] = append(got[cf.Unit], cf.Rules)
			}
			for u := range got {
				sort.Slice(got[u], func(i, j int) bool { return fmtSet(got[u][i]) < fmtSet(got[u][j]) })
			}
			if len(res.Undecided) > 0 {
				t.Errorf("undecided: %v", res.Undecided)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("SMT conflicts %v, want %v", got, c.want)
			}
			model, err := oracle.Build(reg, true)
			if err != nil {
				t.Fatal(err)
			}
			ref := map[string][][]string{}
			for _, u := range model.Units {
				sets, err := model.MinimalConflictSets(u.Name)
				if err != nil {
					t.Fatal(err)
				}
				if len(sets) > 0 {
					ref[u.Name] = sets
				}
			}
			if !reflect.DeepEqual(ref, c.want) {
				t.Errorf("oracle conflicts %v, want %v", ref, c.want)
			}
		})
	}
}

func fmtSet(s []string) string {
	out := ""
	for _, x := range s {
		out += x + " "
	}
	return out
}

// A cached check gives the same result, solving only units that changed.
func TestCacheReusesUnchangedUnits(t *testing.T) {
	src := `context: c
formatVersion: 1
---
entity: Doc
context: c
attributes:
  a: { type: bool }
  b: { type: bool }
---
operation: Doc.x
context: c
default: allow
writes: [Doc.a]
---
operation: Doc.y
context: c
default: allow
writes: [Doc.b]
---
id: R1
kind: invariant
on: Doc.x
assert: self.a
---
id: R2
kind: invariant
on: Doc.x
assert: not self.a
---
id: R3
kind: invariant
on: Doc.y
assert: self.b
`
	run := func(src string, cache *Cache) *Result {
		reg, err := ir.LoadDir(writeRegistry(t, src))
		if err != nil {
			t.Fatal(err)
		}
		prog, err := lower.Lower(reg, lower.Options{})
		if err != nil {
			t.Fatal(err)
		}
		res, err := CheckWith(context.Background(), &solver.Z3{}, prog, Options{Cache: cache})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	cache := &Cache{}
	first := run(src, cache)
	second := run(src, cache)
	if cache.Hits != cache.Misses || !reflect.DeepEqual(first.Conflicts, second.Conflicts) {
		t.Fatalf("hits %d misses %d; %v vs %v", cache.Hits, cache.Misses, first.Conflicts, second.Conflicts)
	}
	misses := cache.Misses
	edited := strings.Replace(src, "assert: self.b", "assert: not self.b", 1)
	third := run(edited, cache)
	if cache.Misses != misses+1 || len(third.Conflicts) != 1 {
		t.Fatalf("an edit to Doc.y should re-check only Doc.y: misses %d, conflicts %v", cache.Misses-misses, third.Conflicts)
	}
}

// A list that declares no length holds every instance a count rule raised
// the bound to: 4 tools, so "the overdue list holds every lent tool" fits.
// At the default length of 3 it read as contradicting itself (a developer
// trial, 2026-11-03).
func TestListsFitInstancesACountRaised(t *testing.T) {
	dir := writeRegistry(t, "context: c\nformatVersion: 1\n---\n"+`
entity: Tool
context: c
attributes:
  id: { type: int, range: [1, 4] }
  lent: { type: bool }
  ann: { type: bool }
---
extend: platform.Result
context: c
attributes:
  overdue: { type: list, of: int, range: [1, 4] }
---
entity: Shed
context: c
attributes:
  open: { type: bool }
---
operation: Shed.overdue
context: c
default: allow
writes: []
---
id: LIMIT
kind: invariant
assert: 'count(t in instances(Tool): t.lent and t.ann) <= 3'
---
id: OD
kind: invariant
on: Shed.overdue
assert: 'all(t in instances(Tool): t.lent implies contains(result.overdue, t.id))'
`)
	reg, err := ir.LoadDir(dir)
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
	if len(res.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v; want none", res.Conflicts)
	}
}
