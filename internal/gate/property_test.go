package gate

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/oracle"
	"github.com/provasign/stele/internal/solver"
)

// Experiment A: random developer histories against an independent oracle.
//
// Each history starts from a small vocabulary (entity E with operations go
// and tick, and a step result) and applies random changes the way developers would: add, edit
// and retire state invariants, step rules, permissions and preconditions;
// grow enums; add attributes; change what an operation writes, its default
// and its effect. When the gate blocks a conflict, the simulated developer
// usually resolves it with a decision record (docs/08 W2). After every gate
// run the test asserts, against brute-force enumeration:
//
//	P1 accepted  => no unit has a conflict set or a dead response
//	P2 rejected  => every reported conflict really has no response in its
//	               situation, every dead response is unreachable under its
//	               core, and per unit the reported conflicts are exactly the
//	               minimal conflict sets
//	P3 no silent meaning change: an unchanged rule whose variables, or their
//	               domains, differ in some unit is listed for approval
//	P4 conservative growth (only new attributes) never changes the verdict
//
// The oracle never slices, so P1 and P2 also test that slicing is sound.
// STELE_PROPERTY_SEEDS and STELE_PROPERTY_STEPS scale the run.

type genRule struct {
	ID, Kind, Effect, Op, When, Assert, Status string
	Version                                    int
}

type genDecision struct {
	ID            string
	Pins          map[string]int // rule -> pinned version
	Winner, Loser string
	When          string
	Retire        string
}

type world struct {
	events, states []string // Request.event (per step) and E.s (written) values
	envBools       []string // extra environment attributes on E
	sysBools       []string // extra attributes on E, written by tick
	goDefault      string
	goWritesQ      bool
	goEffect       string // a value of s that go always sets, or ""
	machine        bool   // E's state machine: go moves s from x to y
	rules          []*genRule
	decisions      []*genDecision
	next           int
}

func (w *world) clone() *world {
	c := *w
	c.events = slices.Clone(w.events)
	c.states = slices.Clone(w.states)
	c.envBools = slices.Clone(w.envBools)
	c.sysBools = slices.Clone(w.sysBools)
	c.rules = nil
	for _, r := range w.rules {
		x := *r
		c.rules = append(c.rules, &x)
	}
	c.decisions = nil
	for _, d := range w.decisions {
		x := *d
		x.Pins = map[string]int{}
		for k, v := range d.Pins {
			x.Pins[k] = v
		}
		c.decisions = append(c.decisions, &x)
	}
	return &c
}

func (w *world) rule(id string) *genRule {
	for _, r := range w.rules {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func (w *world) live() []*genRule {
	var out []*genRule
	for _, r := range w.rules {
		if r.Status == "active" {
			out = append(out, r)
		}
	}
	return out
}

func (w *world) yaml() string {
	var b strings.Builder
	b.WriteString("context: c\nformatVersion: 1\n---\nextend: platform.Request\ncontext: c\nattributes:\n")
	fmt.Fprintf(&b, "  event: { type: enum, values: [%s] }\n", strings.Join(w.events, ", "))
	b.WriteString("---\nextend: platform.Result\ncontext: c\nattributes:\n  ok: { type: bool }\n  tries: { type: int, range: [0, 3] }\n")
	b.WriteString("---\nentity: E\ncontext: c\nattributes:\n  n: { type: int, range: [0, 2] }\n  flag: { type: bool }\n")
	fmt.Fprintf(&b, "  s: { type: enum, values: [%s] }\n", strings.Join(w.states, ", "))
	b.WriteString("  p: { type: bool }\n  q: { type: bool }\n")
	for _, a := range w.envBools {
		fmt.Fprintf(&b, "  %s: { type: bool }\n", a)
	}
	for _, a := range w.sysBools {
		fmt.Fprintf(&b, "  %s: { type: bool }\n", a)
	}
	if w.machine {
		b.WriteString("stateMachine:\n  field: s\n  transitions:\n    - { from: [x], to: y, via: go }\n")
	}
	goWrites := []string{"E.s", "E.p"}
	if w.goWritesQ {
		goWrites = append(goWrites, "E.q")
	}
	fmt.Fprintf(&b, "---\noperation: E.go\ncontext: c\ndefault: %s\nwrites: [%s]\n", w.goDefault, strings.Join(goWrites, ", "))
	if w.goEffect != "" {
		fmt.Fprintf(&b, "effect: { s: %s }\n", w.goEffect)
	}
	tickWrites := []string{"E.s", "E.q"}
	for _, a := range w.sysBools {
		tickWrites = append(tickWrites, "E."+a)
	}
	fmt.Fprintf(&b, "---\noperation: E.tick\ncontext: c\ndefault: allow\nwrites: [%s]\n", strings.Join(tickWrites, ", "))
	for _, r := range w.rules {
		fmt.Fprintf(&b, "---\nid: %s\nversion: %d\nkind: %s\nstatus: %s\n", r.ID, r.Version, r.Kind, r.Status)
		switch {
		case r.Kind == ir.Invariant && r.Op == "":
			b.WriteString("subject: E\n")
		case r.Kind == ir.Invariant:
			fmt.Fprintf(&b, "on: %s\n", r.Op)
		default:
			fmt.Fprintf(&b, "operation: %s\n", r.Op)
		}
		if r.Effect != "" {
			fmt.Fprintf(&b, "effect: %s\n", r.Effect)
		}
		if r.When != "" {
			fmt.Fprintf(&b, "when: %q\n", r.When)
		}
		if r.Assert != "" {
			fmt.Fprintf(&b, "assert: %q\n", r.Assert)
		}
	}
	for _, d := range w.decisions {
		fmt.Fprintf(&b, "---\nid: %s\n", d.ID)
		var pins []string
		for r, v := range d.Pins {
			pins = append(pins, fmt.Sprintf("%s@%d", r, v))
		}
		sort.Strings(pins)
		if d.Retire != "" {
			fmt.Fprintf(&b, "retire: [%s]\n", strings.Join(pins, ", "))
			continue
		}
		fmt.Fprintf(&b, "resolves: [%s]\nsupersede: { winner: %s, loser: %s, when: %q }\n", strings.Join(pins, ", "), d.Winner, d.Loser, d.When)
	}
	return b.String()
}

type gen struct{ rng *rand.Rand }

func (g gen) pick(xs []string) string { return xs[g.rng.IntN(len(xs))] }

// envAtom is a condition on attributes no operation writes. root is "self"
// in rules and "e" in decisions, which have no subject.
func (g gen) envAtom(w *world, root string) string {
	switch g.rng.IntN(8 + len(w.envBools)) {
	case 4:
		return "max(" + root + ".n, 1) = 2"
	case 5:
		return "abs(" + root + ".n - 1) = 1"
	case 6:
		return "min(" + root + ".n, 1) = 0"
	case 7:
		return "seconds(" + root + ".n) < " + strconv.Itoa(1+g.rng.IntN(2)) + "s"
	case 0:
		return root + ".n >= " + strconv.Itoa(1+g.rng.IntN(2))
	case 1:
		return root + ".n < " + strconv.Itoa(1+g.rng.IntN(2))
	case 2:
		return "not " + root + ".flag"
	case 3:
		return root + ".flag"
	default:
		return root + "." + g.pick(w.envBools)
	}
}

// sysAtom is a condition on attributes operations write.
func (g gen) sysAtom(w *world) string {
	switch g.rng.IntN(6 + len(w.sysBools)) {
	case 0:
		return "self.s = " + g.pick(w.states)
	case 1:
		return "self.s != " + g.pick(w.states)
	case 2:
		a, b := g.pick(w.states), g.pick(w.states)
		if a == b {
			return "self.s = " + a
		}
		return "self.s in {" + a + ", " + b + "}"
	case 3:
		return "self.p"
	case 4:
		return "not self.q"
	case 5:
		return "self.q"
	default:
		return "self." + g.pick(w.sysBools)
	}
}

// guard is a condition before a step: on any attribute, and on the request
// when step is set.
func (g gen) guard(w *world, step bool) string {
	atom := func() string {
		switch n := g.rng.IntN(6); {
		case n < 2 && step:
			return "request.event = " + g.pick(w.events)
		case n < 4:
			return g.envAtom(w, "self")
		default:
			return g.sysAtom(w)
		}
	}
	switch g.rng.IntN(6) {
	case 0:
		return ""
	case 1, 2:
		return atom() + " and " + atom()
	}
	return atom()
}

// assert is what a rule requires: in a rule on a step (step set), sometimes
// of the step's result, and in a state invariant sometimes of the other
// instances (D-064).
func (g gen) assert(w *world, step bool) string {
	n := g.rng.IntN(9)
	switch {
	case n == 7 && step:
		return []string{"result.ok", "not result.ok", "result.tries <= 1", "result.tries >= 2"}[g.rng.IntN(4)]
	case n == 8 && step:
		return g.sysAtom(w) + " and result.tries = " + strconv.Itoa(g.rng.IntN(4))
	case n == 7:
		return []string{"count(o in instances(E): o.p) <= 1", "all(o in instances(E): o.p implies not self.p)", "any(o in instances(E): o.p)"}[g.rng.IntN(3)]
	}
	switch n {
	case 0:
		return g.sysAtom(w) + " or " + g.sysAtom(w)
	case 1:
		return g.sysAtom(w) + " and " + g.sysAtom(w)
	case 2:
		return g.sysAtom(w) + " or " + g.envAtom(w, "self")
	}
	return g.sysAtom(w)
}

func (g gen) newRule(w *world) *genRule {
	w.next++
	r := &genRule{ID: fmt.Sprintf("R%d", w.next), Version: 1, Status: "active"}
	op := []string{"E.go", "E.tick"}[g.rng.IntN(2)]
	switch n := g.rng.IntN(10); {
	case n < 4:
		r.Kind, r.When, r.Assert = ir.Invariant, g.guard(w, false), g.assert(w, false)
	case n < 7:
		r.Kind, r.Op, r.When, r.Assert = ir.Invariant, op, g.guard(w, true), g.assert(w, true)
	case n < 9:
		r.Kind, r.Op, r.Effect, r.When = ir.Permission, op, []string{"allow", "deny"}[g.rng.IntN(2)], g.guard(w, true)
	default:
		r.Kind, r.Op, r.When = ir.Precondition, op, g.guard(w, true)
		if r.When == "" {
			r.When = g.sysAtom(w)
		}
	}
	return r
}

func (g gen) edit(w *world, r *genRule) {
	r.Version++
	switch {
	case r.Kind == ir.Invariant && r.Op == "":
		r.When, r.Assert = g.guard(w, false), g.assert(w, false)
	case r.Kind == ir.Invariant:
		r.When, r.Assert = g.guard(w, true), g.assert(w, true)
	case r.Kind == ir.Permission:
		r.When = g.guard(w, true)
	default:
		if r.When = g.guard(w, true); r.When == "" {
			r.When = g.sysAtom(w)
		}
	}
}

// mutate applies one random developer change and names it.
func (g gen) mutate(w *world) string {
	live := w.live()
	switch n := g.rng.IntN(100); {
	case n < 42 && len(live) < 7, len(live) == 0:
		r := g.newRule(w)
		w.rules = append(w.rules, r)
		return "add " + r.ID
	case n < 55:
		r := live[g.rng.IntN(len(live))]
		g.edit(w, r)
		return fmt.Sprintf("edit %s@%d", r.ID, r.Version)
	case n < 64:
		r := live[g.rng.IntN(len(live))]
		r.Status = "retired"
		return "retire " + r.ID
	case n < 70 && len(w.states) < 4:
		v := fmt.Sprintf("v%d", len(w.states))
		w.states = append(w.states, v)
		return "add value E.s = " + v
	case n < 75 && len(w.events) < 4:
		v := fmt.Sprintf("e%d", len(w.events))
		w.events = append(w.events, v)
		return "add value Request.event = " + v
	case n < 80 && len(w.sysBools) < 1:
		w.sysBools = append(w.sysBools, "r0")
		return "add attribute E.r0, written by tick"
	case n < 85 && len(w.envBools) < 1:
		w.envBools = append(w.envBools, "f0")
		return "add environment attribute E.f0"
	case n < 90:
		w.goDefault = map[string]string{"allow": "deny", "deny": "allow"}[w.goDefault]
		return "E.go default " + w.goDefault
	case n < 93:
		w.goWritesQ = !w.goWritesQ
		return fmt.Sprintf("E.go writes q: %v", w.goWritesQ)
	case n < 96:
		w.machine = !w.machine
		return fmt.Sprintf("E state machine (go: x -> y): %v", w.machine)
	default:
		if w.goEffect == "" {
			w.goEffect = g.pick(w.states)
		} else {
			w.goEffect = ""
		}
		return "E.go effect s = " + w.goEffect
	}
}

// resolve writes a decision for the first finding, the way a developer
// answers a blocked PR: retire one rule, or let one supersede another under
// a condition.
func (g gen) resolve(w *world, res *Result) string {
	var core []string
	switch {
	case len(res.Check.Conflicts) > 0:
		core = res.Check.Conflicts[0].Rules
	case len(res.Check.Dead) > 0:
		core = res.Check.Dead[0].Rules
	}
	var mine []string // the operation's effect is vocabulary, not a rule
	for _, id := range core {
		if w.rule(id) != nil {
			mine = append(mine, id)
		}
	}
	if len(mine) == 0 {
		return ""
	}
	w.next++
	id := fmt.Sprintf("D%d", w.next)
	loser := w.rule(mine[g.rng.IntN(len(mine))])
	if len(mine) == 1 || loser.Kind == ir.Precondition || g.rng.IntN(3) == 0 {
		// Preconditions cannot be superseded (D-024); retire is always allowed.
		w.decisions = append(w.decisions, &genDecision{ID: id, Retire: loser.ID, Pins: map[string]int{loser.ID: loser.Version}})
		return "retire " + loser.ID + " by " + id
	}
	var winner *genRule
	for _, r := range mine {
		if r != loser.ID {
			winner = w.rule(r)
			break
		}
	}
	when := g.envAtom(w, "e")
	w.decisions = append(w.decisions, &genDecision{ID: id, Winner: winner.ID, Loser: loser.ID, When: when,
		Pins: map[string]int{winner.ID: winner.Version, loser.ID: loser.Version}})
	return fmt.Sprintf("%s: %s supersedes %s when %s", id, winner.ID, loser.ID, when)
}

// repin updates decisions after an edit made them stale, as the owners would
// after re-approving them.
func (w *world) repin() bool {
	changed := false
	for _, d := range w.decisions {
		for r := range d.Pins {
			if v := w.rule(r).Version; d.Pins[r] != v {
				d.Pins[r] = v
				changed = true
			}
		}
	}
	return changed
}

func write(t *testing.T, w *world) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(w.yaml()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

type stats struct {
	steps, accepted, rejectedCheck, rejectedErrors, resolved, approvals, conservative, stepConflicts int
}

func (s *stats) add(o stats) {
	s.steps += o.steps
	s.accepted += o.accepted
	s.rejectedCheck += o.rejectedCheck
	s.rejectedErrors += o.rejectedErrors
	s.resolved += o.resolved
	s.approvals += o.approvals
	s.conservative += o.conservative
	s.stepConflicts += o.stepConflicts
}

func TestRandomHistories(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	seeds, steps := envInt("STELE_PROPERTY_SEEDS", 4), envInt("STELE_PROPERTY_STEPS", 12)
	var (
		total stats
		mu    sync.Mutex
	)
	t.Cleanup(func() {
		t.Logf("%d gate runs: %d accepted, %d blocked by the check (%d step conflicts), %d blocked by errors; %d resolutions; %d needing approval; %d conservative-only",
			total.steps, total.accepted, total.rejectedCheck, total.stepConflicts, total.rejectedErrors, total.resolved, total.approvals, total.conservative)
	})
	for seed := 1; seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			t.Parallel()
			var st stats
			defer func() { mu.Lock(); total.add(st); mu.Unlock() }()
			g := gen{rng: rand.New(rand.NewPCG(uint64(seed), 7))}
			base := &world{events: []string{"a", "b"}, states: []string{"x", "y", "z"}, goDefault: []string{"allow", "deny"}[seed%2]}
			baseDir := write(t, base)
			var history []string
			for i := 0; i < steps; i++ {
				head := base.clone()
				history = append(history, g.mutate(head))
				res := runStep(t, base, head, baseDir, history, &st)
				for attempt := 0; !res.Accepted && attempt < 3; attempt++ {
					var what string
					switch {
					case len(res.Errors) > 0 && head.repin():
						what = "re-pin stale decisions"
					case res.Check != nil && g.rng.IntN(10) < 8:
						what = g.resolve(head, res)
					}
					if what == "" {
						break
					}
					history = append(history, "  "+what)
					st.resolved++
					res = runStep(t, base, head, baseDir, history, &st)
				}
				if res.Accepted {
					base, baseDir = head, write(t, head)
				} else {
					history = append(history, "  (change abandoned)")
				}
			}
		})
	}
}

func runStep(t *testing.T, base, head *world, baseDir string, history []string, st *stats) *Result {
	t.Helper()
	headDir := write(t, head)
	res, err := Dirs(context.Background(), &solver.Z3{}, baseDir, headDir)
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf(format+"\nhistory:\n%s\nhead registry:\n%s", append(args, strings.Join(history, "\n"), head.yaml())...)
	}
	if err != nil {
		fail("gate: %v", err)
	}
	st.steps++
	switch {
	case res.Accepted:
		st.accepted++
	case len(res.Errors) > 0:
		st.rejectedErrors++
	default:
		st.rejectedCheck++
	}
	if len(res.NeedsApproval) > 0 {
		st.approvals++
	}
	if len(res.Errors) > 0 {
		// The generator writes only well-typed rules and never deletes or
		// edits without a bump, so the only expected errors are stale decisions.
		for _, e := range res.Errors {
			if !strings.Contains(e, "stale") {
				fail("unexpected gate error: %s", e)
			}
		}
		return res
	}

	headReg, err := ir.LoadDir(headDir)
	if err != nil {
		fail("head does not load: %v", err)
	}
	om, err := oracle.Build(headReg, true)
	if err != nil {
		fail("oracle: %v", err)
	}

	// Step units slice the state invariants to those connected to their rules.
	// When the invariants are satisfiable together, that changes no verdict:
	// unconnected invariants share no variables, so any situation extends to
	// satisfy them. When they are not, the state unit reports it, and step
	// units are compared only by their own soundness (P2).
	stateSets, err := om.MinimalConflictSets("state")
	if err != nil {
		fail("oracle: %v", err)
	}
	if om.Unit("assumptions") != nil {
		aSets, err := om.MinimalConflictSets("assumptions")
		if err != nil {
			fail("oracle: %v", err)
		}
		stateSets = append(stateSets, aSets...)
	}
	stateOK := len(stateSets) == 0

	// P1/P2: per unit, the conflicts are exactly the oracle's minimal sets.
	got := map[string][]string{}
	for _, c := range res.Check.Conflicts {
		got[c.Unit] = append(got[c.Unit], fmt.Sprint(c.Rules))
		if c.Unit != "state" {
			st.stepConflicts++
		}
		given := map[string]string{}
		for _, b := range c.Counterexample {
			given[b.Name] = b.Value
		}
		if c.Unit == "state" || c.Unit == "assumptions" || stateOK {
			ok, err := om.ConflictsAt(c.Unit, c.Rules, given)
			if err != nil || !ok {
				fail("P2: reported conflict %v in %s at %v is not a conflict (%v)", c.Rules, c.Unit, c.Counterexample, err)
			}
		}
	}
	consistent := true
	for _, u := range om.Units {
		want, err := om.MinimalConflictSets(u.Name)
		if err != nil {
			fail("oracle: %v", err)
		}
		var ws []string
		for _, s := range want {
			ws = append(ws, fmt.Sprint(s))
		}
		gs := got[u.Name]
		sort.Strings(ws)
		sort.Strings(gs)
		if u.Name != "state" && u.Name != "assumptions" && !stateOK {
			continue
		}
		if fmt.Sprint(gs) != fmt.Sprint(ws) {
			fail("P1/P2: minimal conflict sets differ in unit %s\n  stele:  %v\n  oracle: %v", u.Name, gs, ws)
		}
		consistent = consistent && len(ws) == 0
	}

	// Dead responses agree per unit, and each reported core really kills it.
	// Values one invariant fixes are reported as pinned, not dead, and do
	// not fail the check.
	deadGot, pinned := map[string][]string{}, map[string]bool{}
	for _, d := range res.Pinned {
		pinned[d.Unit+"\x00"+d.Response] = true
	}
	for _, d := range append(append([]checker.DeadResponse{}, res.Check.Dead...), res.Pinned...) {
		deadGot[d.Unit] = append(deadGot[d.Unit], d.Response)
		if d.Unit != "state" && !stateOK {
			continue
		}
		if ok, err := om.Reachable(d.Unit, d.Response, d.Rules); err != nil || ok {
			fail("P2: %s in %s is reachable under its core %v (%v)", d.Response, d.Unit, d.Rules, err)
		}
	}
	for _, u := range om.Units {
		want := om.Dead(u.Name)
		gs := deadGot[u.Name]
		sort.Strings(gs)
		if u.Name != "state" && !stateOK {
			consistent = consistent && len(gs) == 0
			continue
		}
		if fmt.Sprint(gs) != fmt.Sprint(want) {
			fail("P1/P2: dead responses differ in unit %s\n  stele:  %v\n  oracle: %v", u.Name, gs, want)
		}
		for _, w := range want {
			consistent = consistent && pinned[u.Name+"\x00"+w]
		}
	}
	if !stateOK {
		consistent = false // the state unit's conflicts block the merge
	}
	if res.Accepted != consistent {
		fail("P1: accepted=%v but the oracle says consistent=%v", res.Accepted, consistent)
	}

	checkMeaning(base, baseDir, headReg, om, res, fail, st, pinned)
	return res
}

// checkMeaning asserts P3 and P4.
func checkMeaning(base *world, baseDir string, headReg *ir.Registry, om *oracle.Model, res *Result, fail func(string, ...any), st *stats, pinned map[string]bool) {
	baseReg, err := ir.LoadDir(baseDir)
	if err != nil {
		fail("base: %v", err)
	}
	bm, err := oracle.Build(baseReg, true)
	if err != nil {
		fail("oracle base: %v", err)
	}
	flagged := map[string]bool{}
	for _, c := range res.NeedsApproval {
		for _, r := range c.Affected {
			flagged[r] = true
		}
	}
	// A decision added in this change alters its rules on purpose, in plain view.
	decided := map[string]bool{}
	for _, d := range headReg.Decisions {
		if slices.ContainsFunc(baseReg.Decisions, func(x *ir.Decision) bool { return x.ID == d.ID }) {
			continue
		}
		for _, p := range d.Retire {
			decided[p.Rule] = true
		}
		if d.Supersede != nil {
			decided[d.Supersede.Loser] = true
		}
	}
	unchanged := true
	for _, b := range base.rules {
		h := headReg.Rule(b.ID)
		if decided[b.ID] {
			unchanged = false
			continue
		}
		if h == nil || h.Version != b.Version || b.Status != "active" || h.Status != "active" {
			unchanged = false
			continue
		}
		for _, u := range bm.Units {
			if om.Unit(u.Name) == nil {
				continue
			}
			br, hr := bm.Reads(u.Name, b.ID), om.Reads(u.Name, b.ID)
			changed := fmt.Sprint(br) != fmt.Sprint(hr)
			for _, n := range hr {
				changed = changed || bm.Domain(n) != om.Domain(n)
			}
			if changed && !flagged[b.ID] {
				fail("P3: %s means something different in unit %s (reads %v -> %v), but the gate did not list it for approval", b.ID, u.Name, br, hr)
			}
		}
	}
	// P4: a change that only adds attributes keeps the verdict of the base.
	if unchanged && len(base.rules) == len(headReg.Rules) && onlyAdditions(res) {
		st.conservative++
		baseOK := true
		for _, u := range bm.Units {
			sets, err := bm.MinimalConflictSets(u.Name)
			if err != nil {
				fail("oracle: %v", err)
			}
			baseOK = baseOK && len(sets) == 0
			for _, d := range bm.Dead(u.Name) {
				baseOK = baseOK && pinned[u.Name+"\x00"+d]
			}
		}
		if baseOK != (res.Check.Verdict == checker.Pass) {
			fail("P4: adding attributes changed the verdict (base consistent=%v, head verdict=%s)", baseOK, res.Check.Verdict)
		}
	}
}

func onlyAdditions(res *Result) bool {
	if len(res.Rules) > 0 {
		return false
	}
	for _, c := range res.Vocab {
		if c.Kind != "attribute-added" && c.Kind != "entity-added" && c.Kind != "relation-added" {
			return false
		}
	}
	return len(res.Vocab) > 0
}
