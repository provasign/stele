// Package lower turns a registry into SMT-LIB 2 terms over a transition
// system (D-042). The state is every attribute; an operation is one step that
// an actor invokes and the system handles. The program has one unit per
// question the checker asks:
//
//   - the state unit: can every situation be in a state that satisfies the
//     state invariants? Environment attributes (written by no operation) are
//     the situation, written attributes the response.
//   - one step unit per operation: from any state satisfying the invariants,
//     with any request, is there a way to handle the operation that satisfies
//     every rule about it and leaves the invariants true? The state before and
//     the request are the situation; whether the operation runs, and the
//     values of the attributes it writes afterwards, are the response.
//
// In a step unit, rules lower as follows (pre = before, post = after; an
// attribute the operation does not write has the same value in both):
//
//	permission  allow op when G       G(pre) => runs
//	permission  deny op when G        G(pre) => not runs
//	precondition op when P            runs => P(pre)
//	invariant   on op when W assert A runs => (W(pre) => A(post))
//	operation   effect {a: v}         runs => a(post) = v
//	state machine from F via op       runs => field(pre) in F, and op's effect sets the to state
//	invariant   when W assert A       runs => (W => A)(post), if A or W reads
//	                                  something op writes; else it holds by the frame
//	operation   default allow|deny    when no allow (deny) matches and every
//	                                  precondition holds, runs = default
//	frame                             not runs => post = pre
//	decision    supersede L when C    not C(pre) => L
//	decision    retire L              L is dropped
//
// A rule that reads nothing any operation writes is an assumption about the
// situation. A declared default `X.default = v` is one only with
// Options.AssumeDefaults: it is the value when nothing sets X, not the only one.
package lower

import (
	"fmt"
	"regexp/syntax"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/provasign/stele/internal/ir"
)

// Var is one SMT constant.
type Var struct {
	Name   string // SMT symbol, already quoted
	Label  string // human name: Workspace.status, Workspace.status' (after), Workspace.stop:runs
	Sort   string
	System bool   // part of the response this unit asks about
	Range  string // SMT bound over the variable, or ""
	Attr   *ir.Attr
	Post   bool // the value after the step
}

// Role says how a formula takes part in an operation's default.
type Role int

const (
	Plain Role = iota
	Allow
	Deny
	Pre
)

// Formula is one lowered rule in one unit.
type Formula struct {
	Rule  *ir.Rule
	Term  string
	Vars  []*Var // variables the term mentions, sorted by label
	Note  string // set when a decision changed the rule
	Home  bool   // this is the rule's own unit (where it is scored and explained)
	Role  Role
	Guard string // for Allow, Deny and Pre: the guard term, for the default
	// Asserted are the variables the rule's assert (or a default) mentions:
	// what the requirement obliges, as opposed to the condition it reads.
	Asserted []*Var
	// When is an invariant's condition term ("true" when it has none).
	When string
}

// Condition is the term under which the rule applies: an invariant's when,
// or a permission's or precondition's guard. Empty when it always applies.
func (f *Formula) Condition() string {
	c := f.When
	if c == "" {
		c = f.Guard
	}
	if c == "true" {
		return ""
	}
	return c
}

// Unit is one question for the checker.
type Unit struct {
	Name        string        // "state" or the operation name
	Op          *ir.Operation // nil for the state unit
	Vars        []*Var        // every variable the unit mentions, sorted by label
	Rules       []*Formula    // the rules that can conflict
	Assumptions []*Formula    // facts about the situation
	// Possible are permissions with effect possible: the operation must be
	// able to run in some situation where the guard holds (D-052). Term is
	// the guard and the operation running.
	Possible []*Formula
	frame    []string
}

// Fixed returns the constraints that hold in every query of the unit: the
// frame and the operation's default. The default follows from all of the
// operation's rules, since it applies when no rule in the registry matches;
// it does not change with the subset of rules a query considers, which keeps
// core minimization monotone.
func (u *Unit) Fixed() []string {
	if u.Op == nil {
		return nil
	}
	runs := opVarName(u.Op.Name)
	out := []string{}
	if len(u.frame) > 0 {
		out = append(out, fmt.Sprintf("(=> (not %s) %s)", runs, conj(u.frame)))
	}
	var none, pres []string
	for _, f := range u.Rules {
		switch {
		case f.Role == Pre:
			pres = append(pres, f.Guard)
		case f.Role == Allow && u.Op.Default == "deny", f.Role == Deny && u.Op.Default == "allow":
			none = append(none, fmt.Sprintf("(not %s)", f.Guard))
		}
	}
	if u.Op.Default == "deny" {
		// What must be possible is permitted, though not forced (D-052).
		for _, f := range u.Possible {
			none = append(none, fmt.Sprintf("(not %s)", f.Guard))
		}
	}
	if u.Op.Default == "deny" {
		out = append(out, fmt.Sprintf("(=> %s (not %s))", conj(none), runs))
	} else {
		out = append(out, fmt.Sprintf("(=> %s %s)", conj(append(none, pres...)), runs))
	}
	return out
}

func conj(ts []string) string {
	switch len(ts) {
	case 0:
		return "true"
	case 1:
		return ts[0]
	}
	return "(and " + strings.Join(ts, " ") + ")"
}

// Program is a lowered registry.
type Program struct {
	Datatypes []string // declare-datatypes commands, shared by every unit
	Units     []*Unit  // the state unit first, then operations by name
	Retired   []string // rules removed by decisions
	Skipped   []string // rules not checked, with the reason
	Warnings  []string
}

// Home returns the formulas a rule lowers to in its own unit: the state unit
// for state invariants and defaults, its operation's unit otherwise.
func (p *Program) Home(id string) (*Unit, []*Formula) {
	for _, u := range p.Units {
		var out []*Formula
		for _, group := range [][]*Formula{u.Rules, u.Assumptions} {
			for _, f := range group {
				if f.Home && f.Rule.ID == id {
					out = append(out, f)
				}
			}
		}
		if len(out) > 0 {
			return u, out
		}
	}
	return nil, nil
}

// Notes maps rule IDs to decision notes.
func (p *Program) Notes() map[string]string {
	out := map[string]string{}
	for _, u := range p.Units {
		for _, f := range u.Rules {
			if f.Note != "" {
				out[f.Rule.ID] = f.Note
			}
		}
	}
	return out
}

// RuleCount counts the distinct checked rules.
func (p *Program) RuleCount() int {
	seen := map[string]bool{}
	for _, u := range p.Units {
		for _, f := range u.Rules {
			seen[f.Rule.ID] = true
		}
	}
	return len(seen)
}

// Assumed lists the distinct assumptions, in their home units.
func (p *Program) Assumed() []*Formula {
	var out []*Formula
	for _, u := range p.Units {
		for _, f := range u.Assumptions {
			if f.Home {
				out = append(out, f)
			}
		}
	}
	return out
}

// Options select how the registry is lowered.
type Options struct {
	// IgnoreDecisions lowers the rules as written, the way the case studies
	// show the conflicts before any decision record exists.
	IgnoreDecisions bool
	// AssumeDefaults checks only situations where every configured default
	// holds (the shipped configuration). Off, checks cover every value.
	AssumeDefaults bool
}

type phase int

const (
	before phase = iota
	after
)

type lowerer struct {
	reg   *ir.Registry
	opt   Options
	sorts map[*ir.Attr]string
	prog  *Program
	errs  []string
	// before holds state invariants lowered over the state before a step,
	// which read the same in every step (preInvariant).
	before map[string]*Formula
	// lifted holds state invariants restated over the other instances of
	// their entity (forOtherInstances), nil where one cannot be.
	lifted map[string]*ir.Rule
	// targets holds each operation's Target, and targeted its rules read
	// for the instance it acts on (forTarget).
	targets  map[string]*ir.Binary
	targeted map[[2]string]*ir.Rule

	// per unit
	vars   map[string]*Var
	op     *ir.Operation
	ph     phase
	inRule map[string]*Var
	scope  []binding // quantifier variables, innermost last
}

// Lower lowers reg. It returns an error listing every type error found.
func Lower(reg *ir.Registry, opt Options) (*Program, error) {
	lw := &lowerer{reg: reg, opt: opt, sorts: map[*ir.Attr]string{}, prog: &Program{}, before: map[string]*Formula{}, lifted: map[string]*ir.Rule{}, targets: map[string]*ir.Binary{}, targeted: map[[2]string]*ir.Rule{}}
	lw.prog.Warnings = append(lw.prog.Warnings, reg.Notes...)

	retired := map[string]string{}
	superseded := map[string][]*ir.Decision{}
	if !opt.IgnoreDecisions {
		for _, d := range reg.Decisions {
			for _, p := range d.Retire {
				retired[p.Rule] = d.ID
			}
			if d.Supersede != nil {
				superseded[d.Supersede.Loser] = append(superseded[d.Supersede.Loser], d)
			}
		}
	}
	var live []*ir.Rule
	for _, x := range reg.Rules {
		switch {
		case !x.Live():
			lw.prog.Skipped = append(lw.prog.Skipped, fmt.Sprintf("%s: status %s", x.ID, x.Status))
		case retired[x.ID] != "":
			lw.prog.Retired = append(lw.prog.Retired, fmt.Sprintf("%s: retired by %s", x.ID, retired[x.ID]))
		case x.Kind == ir.Measured || x.Kind == ir.Informal || x.Kind == ir.Obligation:
			lw.prog.Skipped = append(lw.prog.Skipped, fmt.Sprintf("%s: %s rules are tracked, not proved", x.ID, x.Kind))
		case x.Kind == ir.Derivation:
			// defaults are lowered from the attributes
		default:
			live = append(live, x)
		}
	}
	for _, d := range reg.Decisions {
		if s := d.Supersede; s != nil && s.When == nil && !opt.IgnoreDecisions {
			lw.errs = append(lw.errs, fmt.Sprintf("decision %s: supersede needs when (use retire to drop a rule everywhere)", d.ID))
		}
	}

	lw.prog.Units = append(lw.prog.Units, lw.stateUnit(live, superseded, retired))
	ops := make([]string, 0, len(reg.Operations))
	for n := range reg.Operations {
		ops = append(ops, n)
	}
	sort.Strings(ops)
	for _, n := range ops {
		lw.prog.Units = append(lw.prog.Units, lw.stepUnit(reg.Operations[n], live, superseded, retired))
	}

	if len(lw.errs) > 0 {
		seen := map[string]bool{}
		var errs []string
		for _, e := range lw.errs {
			if !seen[e] {
				seen[e] = true
				errs = append(errs, e)
			}
		}
		return nil, fmt.Errorf("type errors:\n  %s", strings.Join(errs, "\n  "))
	}
	return lw.prog, nil
}

func (lw *lowerer) begin(op *ir.Operation) {
	lw.vars, lw.op, lw.ph, lw.inRule = map[string]*Var{}, op, before, map[string]*Var{}
}

func (lw *lowerer) startRule() { lw.inRule = map[string]*Var{} }

// stateInvariant reports whether x is a plain invariant over the state.
func stateInvariant(x *ir.Rule) bool { return x.Kind == ir.Invariant && x.On == "" }

func (lw *lowerer) stateUnit(live []*ir.Rule, superseded map[string][]*ir.Decision, retired map[string]string) *Unit {
	lw.begin(nil)
	u := &Unit{Name: "state"}
	for _, x := range live {
		if !stateInvariant(x) {
			continue
		}
		f := lw.invariant(x, superseded, false)
		if f == nil {
			continue
		}
		f.Home = true
		if mentionsSystem(f.Vars) {
			u.Rules = append(u.Rules, f)
		} else {
			lw.prog.Warnings = append(lw.prog.Warnings, fmt.Sprintf(
				"%s reads nothing any operation writes; treated as an environment assumption", x.ID))
			u.Assumptions = append(u.Assumptions, f)
		}
	}
	u.Assumptions = append(u.Assumptions, lw.defaults(retired, true)...)
	u.Vars = sortedVars(lw.vars)
	return u
}

func (lw *lowerer) stepUnit(op *ir.Operation, live []*ir.Rule, superseded map[string][]*ir.Decision, retired map[string]string) *Unit {
	lw.begin(op)
	u := &Unit{Name: op.Name, Op: op}

	// Rules about this operation.
	for _, x := range live {
		if !x.AppliesTo(op.Name) {
			continue
		}
		x = lw.forTarget(x)
		subject, err := lw.reg.SubjectOf(x)
		if err != nil {
			lw.errs = append(lw.errs, err.Error())
			continue
		}
		if x.Assume {
			lw.ph = before
			f := lw.invariant(x, superseded, false)
			if f != nil {
				u.Assumptions = append(u.Assumptions, f)
			}
			continue
		}
		lw.startRule()
		where := "rule " + x.ID
		runs := lw.opVar(op.Name)
		lw.ph = before
		guard := "true"
		if x.When != nil {
			guard = lw.boolTerm(x.When, subject, where)
		}
		f := &Formula{Rule: x, Home: true}
		switch x.Kind {
		case ir.Permission:
			f.Guard = guard
			switch x.Effect {
			case "deny":
				f.Role, f.Term = Deny, implies(guard, "(not "+runs.Name+")")
			case "possible":
				f.Role, f.Term = Plain, fmt.Sprintf("(and %s %s)", guard, runs.Name)
				lw.ph = before
				lw.supersede(f, superseded[x.ID])
				f.Vars = sortedVars(lw.inRule)
				u.Possible = append(u.Possible, f)
				continue
			default:
				f.Role, f.Term = Allow, implies(guard, runs.Name)
			}
		case ir.Precondition:
			f.Role, f.Guard, f.Term = Pre, guard, implies(runs.Name, guard)
		case ir.Invariant:
			lw.ph = after
			body := lw.boolTerm(x.Assert, subject, where)
			f.Term = implies(runs.Name, implies(guard, body))
		}
		lw.ph = before
		lw.supersede(f, superseded[x.ID])
		lw.override(f, x)
		f.Vars = sortedVars(lw.inRule)
		u.Rules = append(u.Rules, f)
	}

	// A state machine's start states: a precondition of the operation.
	if op.From != nil {
		lw.startRule()
		runs := lw.opVar(op.Name)
		lw.ph = before
		v := lw.attrVar(op.From.Attr)
		var eqs []string
		for _, s := range op.From.Values {
			eqs = append(eqs, fmt.Sprintf("(= %s %s)", v.Name, lw.enumValue(op.From.Attr, s, &ir.Ident{Name: s}, "operation "+op.Name)))
		}
		guard := "(or " + strings.Join(eqs, " ") + ")"
		if len(eqs) == 1 {
			guard = eqs[0]
		}
		id := op.Name + ".from"
		u.Rules = append(u.Rules, &Formula{
			Rule: &ir.Rule{ID: id, Kind: ir.Precondition, Operation: op.Name, Status: "active", Source: op.File},
			Term: implies(runs.Name, guard), Vars: sortedVars(lw.inRule), Home: true, Role: Pre, Guard: guard,
		})
	}

	// The operation's declared effect.
	if len(op.Effect) > 0 {
		lw.startRule()
		runs := lw.opVar(op.Name)
		lw.ph = after
		var sets []string
		for _, as := range op.Effect {
			a := as.Attr
			if _, none := as.Value.(*ir.NoneLit); none && a.Optional {
				sets = append(sets, fmt.Sprintf("(not %s)", lw.varFor(a.Entity.Name, a, partSet, false).Name))
				continue
			}
			v := lw.attrVar(a)
			sets = append(sets, fmt.Sprintf("(= %s %s)", v.Name, lw.literal(as.Value, a, "operation "+op.Name)))
			if a.Optional {
				sets = append(sets, lw.varFor(a.Entity.Name, a, partSet, false).Name)
			}
		}
		lw.ph = before
		id := op.Name + ".effect"
		u.Rules = append(u.Rules, &Formula{
			Rule: &ir.Rule{ID: id, Kind: ir.Invariant, On: op.Name, Status: "active", Source: op.File},
			Term: implies(runs.Name, conj(sets)), Vars: sortedVars(lw.inRule), Home: true,
		})
	}

	// Running the operation records it in its history (D-051): it has run,
	// once more (up to MaxRuns), and last at the clock's time.
	if h := op.History; h != nil {
		lw.startRule()
		runs := lw.opVar(op.Name)
		lw.ph = before
		count := lw.attrVar(h.Runs).Name
		now := lw.attrVar(ir.Now).Name
		lw.ph = after
		sets := []string{
			lw.attrVar(h.Ran).Name,
			fmt.Sprintf("(= %s (ite (< %s %d) (+ %s 1) %s))", lw.attrVar(h.Runs).Name, count, ir.MaxRuns, count, count),
			fmt.Sprintf("(= %s %s)", lw.attrVar(h.Last).Name, now),
			lw.varFor(h.Last.Entity.Name, h.Last, partSet, false).Name,
		}
		lw.ph = before
		u.Rules = append(u.Rules, &Formula{
			Rule: &ir.Rule{ID: op.Name + ".history", Kind: ir.Invariant, On: op.Name, Status: "active", Source: op.File},
			Term: implies(runs.Name, conj(sets)), Vars: sortedVars(lw.inRule), Home: true,
		})
	}

	// State invariants hold after the step. Only those that read what the
	// operation writes can change; the rest hold by the frame.
	written := map[*ir.Attr]bool{}
	for _, a := range op.Writes {
		written[a] = true
	}
	var pre []*Formula
	for _, x := range live {
		if !stateInvariant(x) {
			continue
		}
		pf := lw.preInvariant(x, superseded)
		if pf == nil {
			continue
		}
		pre = append(pre, pf)
		if lifted := lw.forOtherInstances(x); lifted != nil {
			// The other instances a rule ranges over were in no state the
			// invariants describe: "every tool's id is positive" held for
			// the tool the step acts on and not for the tools a uniqueness
			// or overdue rule counts, so a step could be shown failing in a
			// state the approved rules rule out (ISSUES 81).
			if lf := lw.preInvariant(lifted, superseded); lf != nil {
				pre = append(pre, lf)
			}
		}
		touches := false
		for _, v := range pf.Vars {
			touches = touches || (v.Attr != nil && written[v.Attr])
		}
		if !touches || !mentionsWritten(pf.Vars) {
			continue
		}
		post := lw.invariant(x, superseded, true)
		runs := lw.opVar(op.Name)
		post.Term = implies(runs.Name, post.Term)
		post.Vars = sortedVars(merge(lw.inRule, map[string]*Var{runs.Label: runs}))
		u.Rules = append(u.Rules, post)
	}

	// The state before satisfies the invariants that share variables with
	// the rules above, transitively (a sound slice of all of them).
	u.Assumptions = append(u.Assumptions, slicePre(pre, u.Rules)...)
	for _, f := range lw.defaults(retired, false) {
		u.Assumptions = append(u.Assumptions, f)
	}

	// The unit mentions what its formulas, frame and run flag use: variables
	// made for invariants the slice left out are not part of its question.
	used := map[string]*Var{}
	for _, group := range [][]*Formula{u.Rules, u.Assumptions, u.Possible} {
		for _, f := range group {
			for _, v := range f.Vars {
				used[v.Label] = v
			}
		}
	}
	for _, a := range op.Writes {
		for _, part := range parts(a) {
			lw.ph = before
			b := lw.varFor(a.Entity.Name, a, part, false)
			lw.ph = after
			p := lw.varFor(a.Entity.Name, a, part, false)
			u.frame = append(u.frame, fmt.Sprintf("(= %s %s)", p.Name, b.Name))
			used[b.Label], used[p.Label] = b, p
		}
	}
	// Being able to do something is running with an effect: a step that
	// runs, reports an error and keeps every value it writes was refused.
	// "Stewards may have up to 5 tools lent" passed beside a 3-tool limit
	// read that way, since lending to a steward with 4 still ran (a
	// developer trial, 2026-11-04). Kept is what x = old(x) means, so an
	// unset value stays unset whatever its leftover value. A step that
	// writes nothing has no effect to require.
	if len(u.Possible) > 0 && len(op.Writes) > 0 {
		lw.startRule()
		var kept []string
		for _, a := range op.Writes {
			if a.Entity != op.Entity {
				for _, part := range parts(a) {
					lw.ph = before
					b := lw.varFor(a.Entity.Name, a, part, false)
					lw.ph = after
					p := lw.varFor(a.Entity.Name, a, part, false)
					kept = append(kept, fmt.Sprintf("(= %s %s)", p.Name, b.Name))
					lw.inRule[b.Label], lw.inRule[p.Label] = b, p
				}
				continue
			}
			e, err := ir.ParseExpr(fmt.Sprintf("self.%s = old(self.%s)", a.Name, a.Name))
			if err != nil {
				continue
			}
			lw.ph = after
			kept = append(kept, lw.boolTerm(e, op.Entity, "the effect of "+op.Name))
		}
		lw.ph = before
		for l, v := range lw.inRule {
			used[l] = v
		}
		for _, f := range u.Possible {
			f.Term = fmt.Sprintf("(and %s (not %s))", f.Term, conj(kept))
		}
	}
	lw.ph = before
	runs := lw.opVar(op.Name)
	used[runs.Label] = runs
	// Slots of one entity that share a key are one instance (keys.go).
	u.Assumptions = append(u.Assumptions, lw.keyAgreement(live, used)...)
	u.Vars = sortedVars(used)
	return u
}

// preInvariant is invariant(x, superseded, false) in a step. Over the state
// before a step, a state invariant reads the same in every step: its
// variables are the same, and so are its decisions and the rules that
// override it, unless an overriding rule is about some steps only. It is
// lowered once and shared, read-only, by the steps.
func (lw *lowerer) preInvariant(x *ir.Rule, superseded map[string][]*ir.Decision) *Formula {
	if f, ok := lw.before[x.ID]; ok {
		return f
	}
	f := lw.invariant(x, superseded, false)
	if f == nil {
		return nil
	}
	for _, w := range lw.reg.Overriders(x.Statement()) {
		if w != x && w.Statement() != x.Statement() && !stateInvariant(w) {
			return f
		}
	}
	lw.before[x.ID] = f
	return f
}

// invariant lowers a state invariant in the current unit, reading the state
// after the step when post is set.
func (lw *lowerer) invariant(x *ir.Rule, superseded map[string][]*ir.Decision, post bool) *Formula {
	subject, err := lw.reg.SubjectOf(x)
	if err != nil {
		lw.errs = append(lw.errs, err.Error())
		return nil
	}
	lw.startRule()
	where := "rule " + x.ID
	lw.ph = before
	if post {
		lw.ph = after
	}
	guard := "true"
	if x.When != nil {
		guard = lw.boolTerm(x.When, subject, where)
	}
	inGuard := lw.inRule
	lw.inRule = map[string]*Var{}
	f := &Formula{Rule: x, Term: implies(guard, lw.boolTerm(x.Assert, subject, where)), When: guard}
	f.Asserted = sortedVars(lw.inRule)
	lw.inRule = merge(inGuard, lw.inRule)
	lw.supersede(f, superseded[x.ID])
	lw.override(f, x)
	lw.ph = before
	f.Vars = sortedVars(lw.inRule)
	return f
}

// override weakens f where a rule that overrides x's statement applies in
// this unit, reading that rule's condition in the current phase: the
// precedence the specification states itself (D-052).
func (lw *lowerer) override(f *Formula, x *ir.Rule) {
	if stateInvariant(x) && !lw.readsSystem(x) {
		// A fact about the world, such as the command's text: no requirement
		// prevails over it. Weakened by the limit that overrode the lending
		// request it came with, it read the limit's tool state and became a
		// rule a step could break by the world choosing another text (a
		// developer trial, 2026-11-04).
		return
	}
	for _, w := range lw.reg.Overriders(x.Statement()) {
		if w == x || w.Statement() == x.Statement() {
			continue
		}
		same := (lw.op == nil && stateInvariant(w) && stateInvariant(x)) ||
			(lw.op != nil && w.AppliesTo(lw.op.Name) && x.AppliesTo(lw.op.Name)) ||
			(lw.op != nil && stateInvariant(w) && stateInvariant(x))
		if !same {
			continue
		}
		w = lw.forTarget(w)
		subject, err := lw.reg.SubjectOf(w)
		if err != nil {
			continue
		}
		cond := "true"
		if w.When != nil {
			cond = lw.boolTerm(w.When, subject, "rule "+w.ID)
		}
		if w.Kind == ir.Precondition {
			// A check before the step prevails where it fails: its
			// condition is what must hold, not where it applies. "Ship
			// every order" gives way to "validate that the order is paid
			// first" for an unpaid order, not for a paid one.
			f.Term = fmt.Sprintf("(=> %s %s)", cond, f.Term)
		} else {
			f.Term = fmt.Sprintf("(=> (not %s) %s)", cond, f.Term)
		}
		f.Note = appendNote(f.Note, "overridden by "+w.ID+" as its text states")
	}
}

// readsSystem reports whether x reads an attribute the system controls.
func (lw *lowerer) readsSystem(x *ir.Rule) bool {
	subject, err := lw.reg.SubjectOf(x)
	if err != nil {
		return true
	}
	reads := lw.reg.Reads(x.Assert, subject)
	if x.When != nil {
		reads = append(reads, lw.reg.Reads(x.When, subject)...)
	}
	for _, a := range reads {
		if a.Controller == ir.System {
			return true
		}
	}
	return false
}

// supersede weakens f with each decision's condition, read in the current phase.
func (lw *lowerer) supersede(f *Formula, ds []*ir.Decision) {
	for _, d := range ds {
		if d.Supersede.When == nil {
			continue
		}
		cond := lw.boolTerm(d.Supersede.When, nil, "decision "+d.ID)
		f.Term = fmt.Sprintf("(=> (not %s) %s)", cond, f.Term)
		f.Note = appendNote(f.Note, "superseded by "+d.ID+" where "+d.Supersede.When.String())
	}
}

func (lw *lowerer) defaults(retired map[string]string, home bool) []*Formula {
	var out []*Formula
	for _, e := range sortedEntities(lw.reg) {
		for _, n := range e.AttrNames() {
			a := e.Attrs[n]
			if a.Default == nil {
				continue
			}
			if live := lw.liveDefaults(a, retired); len(live) > 1 {
				// Each declared default states the one default value; the
				// checker finds that they cannot all hold. Assuming any one of
				// them would pick a side.
				for _, d := range live {
					def := lw.reg.Rule(d.By)
					lw.startRule()
					q := a.QName() + ".default"
					v := &Var{Name: quote(q), Label: q, Sort: lw.sortOf(a), Attr: a}
					v.Range = rangeOf(a, v.Name)
					lw.inRule[q] = v
					lit := lw.literal(d.Value, a, "rule "+def.ID)
					out = append(out, &Formula{Rule: def, Term: fmt.Sprintf("(= %s %s)", v.Name, lit), Vars: sortedVars(lw.inRule), Asserted: sortedVars(lw.inRule), Home: home})
				}
				continue
			}
			live := lw.liveDefaults(a, retired)
			if len(live) == 0 {
				continue
			}
			def, value := lw.reg.Rule(live[0].By), live[0].Value
			if a.Controller == ir.System {
				// A default of an attribute some operation writes is its
				// initial value, not a fact about every situation; checks are
				// one step over every state, so there is no initial state to
				// pin it to yet.
				if home {
					lw.prog.Warnings = append(lw.prog.Warnings, fmt.Sprintf(
						"%s: %s is written by an operation, so its default is an initial value; not checked", def.ID, a.QName()))
				}
				continue
			}
			if !lw.opt.AssumeDefaults {
				// A default is the value when nothing sets it. Users and
				// configuration can set another, so checks cover every value:
				// assuming the default would make every rule about the other
				// values vacuous and hide its conflicts.
				continue
			}
			lw.startRule()
			lw.ph = before
			v := lw.attrVar(a)
			lit := lw.literal(value, a, "rule "+def.ID)
			out = append(out, &Formula{Rule: def, Term: fmt.Sprintf("(= %s %s)", v.Name, lit), Vars: sortedVars(lw.inRule), Asserted: sortedVars(lw.inRule), Home: home})
		}
	}
	return out
}

// liveDefaults returns the declared defaults whose rules are live and not
// retired by a decision.
func (lw *lowerer) liveDefaults(a *ir.Attr, retired map[string]string) []ir.DeclaredDefault {
	var out []ir.DeclaredDefault
	for _, d := range a.Defaults {
		if r := lw.reg.Rule(d.By); r != nil && r.Live() && retired[r.ID] == "" {
			out = append(out, d)
		}
	}
	return out
}

// slicePre keeps the pre-state invariants connected to the rules through
// shared variables. Unconnected invariants constrain nothing the rules read.
func slicePre(pre, rules []*Formula) []*Formula {
	reach := map[string]bool{}
	for _, f := range rules {
		for _, v := range f.Vars {
			reach[v.Name] = true
			if v.Post {
				// The frame ties a' to a: an operation that does not run keeps
				// the value, so invariants on a before the step matter too.
				reach[quote(strings.TrimSuffix(v.Label, "'"))] = true
			}
		}
	}
	taken := make([]bool, len(pre))
	for changed := true; changed; {
		changed = false
		for i, f := range pre {
			if taken[i] {
				continue
			}
			for _, v := range f.Vars {
				if reach[v.Name] {
					taken[i], changed = true, true
					for _, w := range f.Vars {
						reach[w.Name] = true
					}
					break
				}
			}
		}
	}
	var out []*Formula
	for i, f := range pre {
		if taken[i] {
			out = append(out, f)
		}
	}
	return out
}

func merge(a, b map[string]*Var) map[string]*Var {
	out := map[string]*Var{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func appendNote(note, s string) string {
	if note == "" {
		return s
	}
	return note + "; " + s
}

func mentionsSystem(vs []*Var) bool {
	for _, v := range vs {
		if v.System {
			return true
		}
	}
	return false
}

func mentionsWritten(vs []*Var) bool {
	for _, v := range vs {
		if v.Attr != nil && v.Attr.Controller == ir.System {
			return true
		}
	}
	return false
}

func sortedEntities(reg *ir.Registry) []*ir.Entity {
	out := make([]*ir.Entity, 0, len(reg.Entities))
	for _, e := range reg.Entities {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedVars(m map[string]*Var) []*Var {
	out := make([]*Var, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func implies(guard, body string) string {
	if guard == "true" {
		return body
	}
	return fmt.Sprintf("(=> %s %s)", guard, body)
}

func quote(s string) string { return "|" + s + "|" }

// sortOf returns the SMT sort for an attribute or element type, declaring a
// datatype per enum.
func (lw *lowerer) sortOf(a *ir.Attr) string {
	switch a.Kind {
	case ir.Bool:
		return "Bool"
	case ir.Int, ir.Duration, ir.Time:
		return "Int"
	case ir.Decimal:
		return "Real"
	case ir.String:
		return "String"
	case ir.List:
		return "(Seq " + lw.sortOf(a.Elem) + ")"
	case ir.SetKind:
		if a.Bounded() {
			return "(Seq " + lw.sortOf(a.Elem) + ")"
		}
		return "(Array " + lw.sortOf(a.Elem) + " Bool)"
	case ir.Map:
		return "(Array " + lw.sortOf(a.Key) + " " + lw.sortOf(a.Elem) + ")"
	}
	if s, ok := lw.sorts[a]; ok {
		return s
	}
	s := quote(a.QName() + ":T")
	lw.sorts[a] = s
	if len(a.Values) == 0 {
		lw.errs = append(lw.errs, fmt.Sprintf("%s: enum has no values (extend it in a context you own)", a.QName()))
		return s
	}
	ctors := make([]string, len(a.Values))
	for i, v := range a.Values {
		ctors[i] = "(" + EnumConst(a, v) + ")"
	}
	lw.prog.Datatypes = append(lw.prog.Datatypes,
		fmt.Sprintf("(declare-datatypes ((%s 0)) ((%s)))", s, strings.Join(ctors, " ")))
	return s
}

// EnumConst is the SMT constructor for value v of enum attribute a.
func EnumConst(a *ir.Attr, v string) string { return quote(a.QName() + ":" + v) }

// rangeOf bounds a variable of type a, or returns "".
func rangeOf(a *ir.Attr, name string) string {
	switch {
	case a.Kind == ir.Int && a.HasRange:
		return fmt.Sprintf("(and (<= %s %s) (<= %s %s))", smtInt(a.Min), name, name, smtInt(a.Max))
	case a.Kind == ir.Decimal && a.HasRange:
		return fmt.Sprintf("(and (<= %s %s) (<= %s %s))", smtReal(a.RMin), name, name, smtReal(a.RMax))
	}
	return ""
}

// Parts of an attribute with more than one variable: an optional value's
// presence and a map's present keys.
const (
	partValue = ""
	partSet   = "?"
	partKeys  = "#keys"
	partLen   = "#n" // a list's or bounded set's length; its elements are #0, #1, ...
)

// attrVar is the variable for a's single instance in the current unit and
// phase. In a step unit, an attribute the operation writes has a value after
// the step; every other attribute is the same before and after.
func (lw *lowerer) attrVar(a *ir.Attr) *Var { return lw.varFor(a.Entity.Name, a, partValue, false) }

// kindRelation types a path ending at an optional relation: it can only be
// compared with none or tested with isSet.
const kindRelation ir.Kind = 100

// varFor is the variable for part of attribute a of the instance at prefix:
// an entity name for its single instance, or a slot of a relation to many
// such as Node.workspaces#1. Slots are other instances than the ones the
// operation acts on, so the operation never writes them.
func (lw *lowerer) varFor(prefix string, a *ir.Attr, part string, slot bool) *Var {
	key, post := prefix+"."+a.Name+part, false
	if !slot && lw.op != nil && lw.ph == after && lw.op.WritesAttr(a) {
		key, post = key+"'", true
	}
	output := a.Entity != nil && a.Entity.Name == "Result"
	if output && (lw.op == nil || lw.ph != after) {
		lw.errs = append(lw.errs, fmt.Sprintf("result.%s is what a step produces: read it only in the assert of a rule on an operation", a.Name))
	}
	v, ok := lw.vars[key]
	if !ok {
		system := a.Controller == ir.System
		if lw.op != nil {
			system = post // before a step, all of the state is given
		}
		if output {
			system = true // the step's own result, fresh each step: no frame keeps it
		}
		sort := "Bool"
		elem := slotted(a) && strings.HasPrefix(part, "#") && part != partLen && part != partKeys
		switch {
		case part == partValue:
			sort = lw.sortOf(a)
		case part == partKeys:
			sort = "(Array " + lw.sortOf(a.Key) + " Bool)"
		case part == partLen:
			sort = "Int"
		case elem:
			sort = lw.sortOf(a.Elem)
		}
		v = &Var{Name: quote(key), Label: key, Sort: sort, System: system, Attr: a, Post: post}
		switch {
		case part == partValue:
			v.Range = rangeOf(a, v.Name)
		case part == partLen:
			v.Range = fmt.Sprintf("(and (<= 0 %s) (<= %s %d))", v.Name, v.Name, a.MaxLen)
		case elem:
			v.Range = rangeOf(a.Elem, v.Name)
		}
		lw.vars[key] = v
	}
	lw.inRule[key] = v
	return v
}

// flagVar is a Bool of the situation that no attribute owns: a slot's
// membership in a relation to many.
func (lw *lowerer) flagVar(key string) *Var {
	v, ok := lw.vars[key]
	if !ok {
		v = &Var{Name: quote(key), Label: key, Sort: "Bool"}
		lw.vars[key] = v
	}
	lw.inRule[key] = v
	return v
}

// slotted reports whether a is held as a length and one variable per
// position: a list, or a set of strings, numbers or times. This keeps checks
// out of the sequence theory, which the solver does not decide.
func slotted(a *ir.Attr) bool { return a.Kind == ir.List || a.Bounded() }

func elemPart(i int) string { return fmt.Sprintf("#%d", i) }

// parts lists the variable parts of a: its value (or length and elements),
// and its presence or keys.
func parts(a *ir.Attr) []string {
	out := []string{partValue}
	if slotted(a) {
		out = []string{partLen}
		for i := 0; i < a.MaxLen; i++ {
			out = append(out, elemPart(i))
		}
	}
	if a.Optional {
		out = append(out, partSet)
	}
	if a.Kind == ir.Map {
		out = append(out, partKeys)
	}
	return out
}

func opVarName(op string) string { return quote(op + ":runs") }

// opVar is the system's choice to run the unit's operation.
func (lw *lowerer) opVar(op string) *Var {
	key := op + ":runs"
	v, ok := lw.vars[key]
	if !ok {
		v = &Var{Name: opVarName(op), Label: key, Sort: "Bool", System: true}
		lw.vars[key] = v
	}
	if lw.inRule != nil {
		lw.inRule[key] = v
	}
	return v
}

func smtInt(n int) string {
	if n < 0 {
		return "(- " + strconv.Itoa(-n) + ")"
	}
	return strconv.Itoa(n)
}

// smtReal writes a decimal literal as an SMT real.
func smtReal(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	if strings.HasPrefix(s, ".") {
		s = "0" + s
	}
	if neg {
		return "(- " + s + ")"
	}
	return s
}

// smtString writes a string literal in SMT-LIB 2.6 syntax.
func smtString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`""`)
		case r < 0x20 || r > 0x7e || r == '\\':
			fmt.Fprintf(&b, `\u{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// typed is a lowered term and its type.
type typed struct {
	term string
	kind ir.Kind
	// t is the term's declared type (an attribute or a collection's element
	// type) when it has one; enum terms always have it, as their domain.
	t   *ir.Attr
	lit string // a bare enum literal, typed by the other operand
	// numLit marks an integer literal, which also fits decimals.
	numLit bool
	none   bool    // the literal none
	items  []typed // a list literal's elements, typed by the other operand
	// length and elems hold a slotted collection: its length and one term
	// per position up to its bound.
	length string
	elems  []string
	set    string // an optional value's presence term
	keys   string // a map's present keys term
	coll   *slots // a relation to many
}

// enum is the enum domain of t, or nil.
func (t typed) enum() *ir.Attr {
	if t.kind == ir.Enum {
		return t.t
	}
	return nil
}

// slots is a relation to many seen from one instance.
type slots struct {
	rel  *ir.Relation
	from place
}

// place is an instance: a single one (the entity's name), or a slot.
type place struct {
	entity *ir.Entity
	prefix string
	slot   bool
	// via is the relation a slot belongs to and the place it belongs to, so
	// that the slot's inverse relation leads back there.
	via   *ir.Relation
	owner *place
	// cond holds when the optional relations walked to reach the instance
	// all lead somewhere; values read there are unset otherwise.
	cond string
}

// presence is the term saying every optional operand has a value, or "".
func presence(ts ...typed) string {
	var parts []string
	for _, t := range ts {
		if t.set != "" && !slices.Contains(parts, t.set) {
			parts = append(parts, t.set)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return conj(parts)
}

// guard makes a condition false where an operand it reads has no value: an
// unset optional value equals nothing and compares with nothing (D-052).
func guard(set, term string) string {
	if set == "" {
		return term
	}
	return "(and " + set + " " + term + ")"
}

func andTerms(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return "(and " + a + " " + b + ")"
}

// binding is a quantifier variable: an element value, or a slot.
type binding struct {
	name string
	val  *typed
	at   *place
}

func (lw *lowerer) lookup(name string) *binding {
	for i := len(lw.scope) - 1; i >= 0; i-- {
		if lw.scope[i].name == name {
			return &lw.scope[i]
		}
	}
	return nil
}

func (lw *lowerer) boolTerm(e ir.Expr, subject *ir.Entity, where string) string {
	t := lw.expr(e, subject, where)
	if t.kind != ir.Bool || t.lit != "" || t.coll != nil || t.none {
		lw.errs = append(lw.errs, fmt.Sprintf("%s: %s is not a condition", where, e))
	}
	return guard(t.set, t.term) // an unset optional flag does not hold
}

func (lw *lowerer) fail(where, format string, args ...any) typed {
	lw.errs = append(lw.errs, where+": "+fmt.Sprintf(format, args...))
	return typed{term: "false", kind: ir.Bool}
}

// value types the variable of attribute a at an instance.
func (lw *lowerer) value(at place, a *ir.Attr) typed {
	if slotted(a) {
		t := typed{kind: a.Kind, t: a, set: at.cond, length: lw.varFor(at.prefix, a, partLen, at.slot).Name}
		for i := 0; i < a.MaxLen; i++ {
			t.elems = append(t.elems, lw.varFor(at.prefix, a, elemPart(i), at.slot).Name)
		}
		if a.Optional {
			t.set = andTerms(at.cond, lw.varFor(at.prefix, a, partSet, at.slot).Name)
		}
		return t
	}
	t := typed{term: lw.varFor(at.prefix, a, partValue, at.slot).Name, kind: a.Kind, t: a, set: at.cond}
	if a.Optional {
		t.set = andTerms(at.cond, lw.varFor(at.prefix, a, partSet, at.slot).Name)
	}
	if a.Kind == ir.Map {
		t.keys = lw.varFor(at.prefix, a, partKeys, at.slot).Name
	}
	return t
}

// path lowers a path: an attribute of an instance, a quantifier's element,
// or a relation to many (only as a quantifier's collection).
func (lw *lowerer) path(p *ir.Path, subject *ir.Entity, where string) typed {
	var at place
	root := p.Segs[0]
	if b := lw.lookup(root); b != nil {
		if b.val != nil {
			if len(p.Segs) > 1 {
				return lw.fail(where, "%s: %s is a value, not an entity", p, root)
			}
			return *b.val
		}
		at = *b.at
	} else {
		var e *ir.Entity
		switch root {
		case "self":
			e = subject
		case "it":
			return lw.fail(where, "%s: `it` is only valid inside an aggregate", p)
		default:
			// Registry.Resolve names the error for a root it does not know.
			if _, err := lw.reg.ResolveRef(p, subject); err != nil {
				return lw.fail(where, "%v", err)
			}
			e = lw.reg.Entities[lw.rootName(root)]
		}
		if e == nil {
			return lw.fail(where, "%s: `self` has no subject here", p)
		}
		at = place{entity: e, prefix: e.Name}
	}
	segs := p.Segs[1:]
	if len(segs) == 0 {
		return lw.fail(where, "%s: a path must end at an attribute", p)
	}
	for i, s := range segs {
		last := i == len(segs)-1
		cur := at.entity
		if a, ok := cur.Attrs[s]; ok {
			if !last {
				return lw.fail(where, "%s: %s.%s is an attribute, not a relation", p, cur.Name, s)
			}
			return lw.value(at, a)
		}
		rel, ok := cur.Relations[s]
		if !ok {
			return lw.fail(where, "%s: %s has no attribute or relation %q", p, cur.Name, s)
		}
		to := lw.reg.Entities[rel.To]
		relSet := ""
		if rel.Optional {
			relSet = lw.varFor(at.prefix, rel.Set, partValue, at.slot).Name
		}
		if last && rel.Optional {
			return typed{kind: kindRelation, set: andTerms(at.cond, relSet)}
		}
		cond := andTerms(at.cond, relSet)
		switch {
		case rel.Many:
			if !last {
				return lw.fail(where, "%s: %s.%s relates to many %s; range over it with all, any or count", p, cur.Name, s, rel.To)
			}
			return typed{coll: &slots{rel: rel, from: at}}
		case last:
			return lw.fail(where, "%s: path ends at relation %s.%s; name an attribute of %s", p, cur.Name, s, rel.To)
		case rel.Record && at.slot:
			at = place{entity: to, prefix: at.prefix + "." + s, slot: true, via: at.via, owner: at.owner, cond: cond}
		case at.slot && at.via != nil && at.owner != nil && rel.To == at.owner.entity.Name && rel.Inverse == at.via.Name:
			at = *at.owner // back to the instance the slot belongs to
		default:
			at = place{entity: to, prefix: to.Name, cond: cond} // the single instance
		}
	}
	panic("unreachable")
}

// rootName is the entity a lower-camel root names.
func (lw *lowerer) rootName(root string) string {
	switch root {
	case "request":
		return "Request"
	case "actor":
		return "Actor"
	case "result":
		return "Result"
	}
	for name := range lw.reg.Entities {
		if name != "" && strings.ToLower(name[:1])+name[1:] == root {
			return name
		}
	}
	return ""
}

func (lw *lowerer) expr(e ir.Expr, subject *ir.Entity, where string) typed {
	switch e := e.(type) {
	case *ir.BoolLit:
		return typed{term: strconv.FormatBool(e.V), kind: ir.Bool}
	case *ir.IntLit:
		return typed{term: smtInt(e.V), kind: ir.Int, numLit: true}
	case *ir.DecLit:
		return typed{term: smtReal(e.Text), kind: ir.Decimal}
	case *ir.StrLit:
		return typed{term: smtString(e.V), kind: ir.String}
	case *ir.DurLit:
		return typed{term: smtInt(e.MS), kind: ir.Duration}
	case *ir.TimeLit:
		return typed{term: smtInt(e.MS), kind: ir.Time}
	case *ir.NoneLit:
		return typed{none: true}
	case *ir.ListLit:
		t := typed{kind: ir.List, items: []typed{}}
		for _, x := range e.Elems {
			t.items = append(t.items, lw.expr(x, subject, where))
		}
		return t
	case *ir.Ident:
		if e.Name == "now" && lw.lookup("now") == nil {
			return typed{term: lw.attrVar(ir.Now).Name, kind: ir.Time, t: ir.Now}
		}
		if b := lw.lookup(e.Name); b != nil {
			// A quantifier's variable, written alone.
			return lw.path(&ir.Path{Segs: []string{e.Name}}, subject, where)
		}
		return typed{kind: ir.Enum, lit: e.Name}
	case *ir.Path:
		return lw.path(e, subject, where)
	case *ir.Not:
		return typed{term: "(not " + lw.boolTerm(e.X, subject, where) + ")", kind: ir.Bool}
	case *ir.Scale:
		x := lw.expr(e.X, subject, where)
		switch x.kind {
		case ir.Int, ir.Duration:
			return typed{term: fmt.Sprintf("(* %s %s)", smtInt(e.K), x.term), kind: x.kind, set: x.set}
		case ir.Decimal:
			return typed{term: fmt.Sprintf("(* %s %s)", smtReal(strconv.Itoa(e.K)), x.term), kind: ir.Decimal, set: x.set}
		}
		return lw.fail(where, "%s: `*` needs a number", e)
	case *ir.Set:
		return lw.fail(where, "%s: a set is only valid on the right of `in`", e)
	case *ir.Binary:
		return lw.binary(e, subject, where)
	case *ir.Call:
		return lw.call(e, subject, where)
	case *ir.Index:
		m := lw.expr(e.X, subject, where)
		if m.kind == ir.List && m.t != nil {
			// A position: unset past the end.
			i := lw.expr(e.K, subject, where)
			if i.kind != ir.Int {
				return lw.fail(where, "%s: a list position is an int", e)
			}
			in := fmt.Sprintf("(and (<= 0 %s) (< %s %s))", i.term, i.term, m.length)
			term := m.elems[len(m.elems)-1]
			for k := len(m.elems) - 2; k >= 0; k-- {
				term = fmt.Sprintf("(ite (= %s %d) %s %s)", i.term, k, m.elems[k], term)
			}
			return typed{term: term, kind: m.t.Elem.Kind, t: m.t.Elem, set: andTerms(presence(m, i), in)}
		}
		if m.kind != ir.Map || m.t == nil {
			return lw.fail(where, "%s: [] looks up a map attribute or a list position", e)
		}
		kt := lw.expr(e.K, subject, where)
		k, ok := lw.as(kt, m.t.Key, e, where)
		if !ok {
			return typed{term: "false", kind: ir.Bool}
		}
		return typed{term: fmt.Sprintf("(select %s %s)", m.term, k), kind: m.t.Elem.Kind, t: m.t.Elem, set: presence(m, kt)}
	case *ir.Quant:
		return lw.quant(e, subject, where)
	}
	return lw.fail(where, "unsupported expression %s", e)
}

// as lowers x as a value of type t: an enum literal becomes t's constructor
// and an integer becomes a decimal where t is one.
func (lw *lowerer) as(x typed, t *ir.Attr, e ir.Expr, where string) (string, bool) {
	switch {
	case x.lit != "":
		if t.Kind != ir.Enum {
			lw.fail(where, "%s: %q is not a value here (is it a path missing its root?)", e, x.lit)
			return "false", false
		}
		return lw.enumValue(t, x.lit, e, where), true
	case t.Kind == ir.Decimal && x.kind == ir.Int:
		return toReal(x), true
	case x.kind != t.Kind || x.coll != nil || x.none:
		lw.fail(where, "%s: expected a %s", e, ir.KindNames[t.Kind])
		return "false", false
	case t.Kind == ir.Enum && x.t != t:
		if !sameValues(x.t, t) {
			lw.fail(where, "%s: %s and %s have incompatible enum domains; compare each to a value", e, x.t.QName(), t.QName())
			return "false", false
		}
		return lw.recast(x, t), true
	}
	return x.term, true
}

// recast maps an enum term onto another enum sort with the same value names.
func (lw *lowerer) recast(x typed, t *ir.Attr) string {
	term := EnumConst(t, t.Values[len(t.Values)-1])
	for _, v := range t.Values[:len(t.Values)-1] {
		term = fmt.Sprintf("(ite (= %s %s) %s %s)", x.term, lw.enumValue(x.t, v, nil, ""), EnumConst(t, v), term)
	}
	lw.sortOf(t)
	return term
}

func sameValues(a, b *ir.Attr) bool {
	x, y := slices.Clone(a.Values), slices.Clone(b.Values)
	slices.Sort(x)
	slices.Sort(y)
	return len(x) > 0 && slices.Equal(x, y)
}

func toReal(x typed) string {
	if x.numLit {
		return smtReal(strings.Trim(strings.ReplaceAll(x.term, "(- ", "-"), ")"))
	}
	return "(to_real " + x.term + ")"
}

func (lw *lowerer) call(e *ir.Call, subject *ir.Entity, where string) typed {
	if e.Fn == "isSet" || e.Fn == "has" || e.Fn == "old" || ir.HistoryFunctions[e.Fn] {
		return lw.call0(e, subject, where)
	}
	// Other functions read their arguments' values: unset optional values
	// make a condition false and a length unset.
	t := lw.call0(e, subject, where)
	var args []typed
	for _, x := range e.Args {
		args = append(args, lw.expr(x, subject, where))
	}
	if t.kind == ir.Bool {
		t.term = guard(presence(args...), t.term)
	} else {
		t.set = presence(args...)
	}
	return t
}

func (lw *lowerer) call0(e *ir.Call, subject *ir.Entity, where string) typed {
	switch e.Fn {
	case "old":
		// The value before the step, in a rule about an operation.
		ph := lw.ph
		lw.ph = before
		t := lw.expr(e.Args[0], subject, where)
		lw.ph = ph
		return t
	case "ran", "runs", "lastRun":
		p, _ := e.Args[0].(*ir.Path)
		var op *ir.Operation
		if p != nil {
			op = lw.reg.Operations[strings.Join(p.Segs, ".")]
		}
		if op == nil || op.History == nil {
			return lw.fail(where, "%s: %s needs an operation name, Entity.operation", e, e.Fn)
		}
		h := op.History
		switch e.Fn {
		case "ran":
			return typed{term: lw.attrVar(h.Ran).Name, kind: ir.Bool, t: h.Ran}
		case "runs":
			return typed{term: lw.attrVar(h.Runs).Name, kind: ir.Int, t: h.Runs}
		}
		return lw.value(place{entity: op.Entity, prefix: op.Entity.Name}, h.Last)
	}
	args := make([]typed, len(e.Args))
	for i, x := range e.Args {
		args[i] = lw.expr(x, subject, where)
	}
	strs := func() bool {
		for _, a := range args {
			if a.kind != ir.String || a.coll != nil {
				lw.fail(where, "%s: %s takes strings", e, e.Fn)
				return false
			}
		}
		return true
	}
	switch e.Fn {
	case "startsWith", "endsWith":
		if !strs() {
			return typed{term: "false", kind: ir.Bool}
		}
		fn := map[string]string{"startsWith": "str.prefixof", "endsWith": "str.suffixof"}[e.Fn]
		return typed{term: fmt.Sprintf("(%s %s %s)", fn, args[1].term, args[0].term), kind: ir.Bool}
	case "contains":
		c := args[0]
		switch {
		case c.kind == ir.String && c.coll == nil:
			if !strs() {
				return typed{term: "false", kind: ir.Bool}
			}
			return typed{term: fmt.Sprintf("(str.contains %s %s)", c.term, args[1].term), kind: ir.Bool}
		case c.kind == ir.List && c.t != nil:
			v, ok := lw.as(args[1], c.t.Elem, e, where)
			if !ok {
				break
			}
			return typed{term: holds(c, v), kind: ir.Bool}
		case c.kind == ir.SetKind && c.t != nil:
			v, ok := lw.as(args[1], c.t.Elem, e, where)
			if !ok {
				break
			}
			if c.t.Bounded() {
				return typed{term: holds(c, v), kind: ir.Bool}
			}
			return typed{term: fmt.Sprintf("(select %s %s)", c.term, v), kind: ir.Bool}
		default:
			return lw.fail(where, "%s: contains takes a string, list or set first", e)
		}
		return typed{term: "false", kind: ir.Bool}
	case "len":
		c := args[0]
		switch {
		case c.kind == ir.String && c.coll == nil:
			return typed{term: "(str.len " + c.term + ")", kind: ir.Int}
		case c.kind == ir.List:
			return typed{term: c.length, kind: ir.Int}
		case c.kind == ir.SetKind && c.t != nil && c.t.Bounded():
			// Positions may repeat an element: count the first occurrences.
			var terms []string
			for i := range c.elems {
				first := fmt.Sprintf("(< %d %s)", i, c.length)
				for j := 0; j < i; j++ {
					first = fmt.Sprintf("(and %s (not (= %s %s)))", first, c.elems[j], c.elems[i])
				}
				terms = append(terms, fmt.Sprintf("(ite %s 1 0)", first))
			}
			return typed{term: sum(terms), kind: ir.Int}
		case c.kind == ir.SetKind && c.t != nil && (c.t.Elem.Kind == ir.Enum || c.t.Elem.Kind == ir.Bool):
			var terms []string
			for _, v := range lw.members(c.t.Elem) {
				terms = append(terms, fmt.Sprintf("(ite (select %s %s) 1 0)", c.term, v.term))
			}
			return typed{term: sum(terms), kind: ir.Int}
		}
		if c.kind == ir.Map {
			// Said plainly, with what to write instead: told only what len
			// takes, the model's repairs of a listing written as a map ran
			// to the request limit, twice in each of two runs (a developer
			// trial on Codex, 2026-11-06).
			return lw.fail(where, "%s: a map has no length in Stele's rules: say which keys it holds with has(m, k), as all(x in instances(Item): has(m, x.key)), and what it holds for each with m[k]; to say it holds no other keys, use a list or a set of the keys instead", e)
		}
		return lw.fail(where, "%s: len takes a string, a list, or a set of enum or bool values", e)
	case "matches":
		lit, ok := e.Args[1].(*ir.StrLit)
		if !ok || args[0].kind != ir.String {
			return lw.fail(where, "%s: matches takes a string and a regular expression in quotes", e)
		}
		re, err := regexTerm(lit.V)
		if err != nil {
			return lw.fail(where, "%s: %v", e, err)
		}
		return typed{term: fmt.Sprintf("(str.in_re %s %s)", args[0].term, re), kind: ir.Bool}
	case "isEmpty":
		c := args[0]
		switch {
		case c.kind == ir.String && c.coll == nil:
			return typed{term: fmt.Sprintf(`(= %s "")`, c.term), kind: ir.Bool}
		case c.kind == ir.List, c.kind == ir.SetKind && c.t != nil && c.t.Bounded():
			return typed{term: fmt.Sprintf("(= %s 0)", c.length), kind: ir.Bool}
		case c.kind == ir.SetKind && c.t != nil:
			return typed{term: fmt.Sprintf("(= %s ((as const %s) false))", c.term, lw.sortOf(c.t)), kind: ir.Bool}
		case c.kind == ir.Map && c.keys != "":
			return typed{term: fmt.Sprintf("(= %s ((as const (Array %s Bool)) false))", c.keys, lw.sortOf(c.t.Key)), kind: ir.Bool}
		}
		return lw.fail(where, "%s: isEmpty takes a string, list, set or map", e)
	case "seconds", "minutes", "hours", "days":
		// A whole number of units as a duration: exact, and linear.
		if args[0].kind != ir.Int {
			return lw.fail(where, "%s: %s takes a whole number", e, e.Fn)
		}
		ms := map[string]int{"seconds": 1000, "minutes": 60_000, "hours": 3_600_000, "days": 86_400_000}[e.Fn]
		return typed{term: fmt.Sprintf("(* %d %s)", ms, args[0].term), kind: ir.Duration}
	case "min", "max", "abs":
		k, ok := numericKind(args)
		if !ok {
			return lw.fail(where, "%s: %s takes numbers, durations or times of one kind", e, e.Fn)
		}
		terms := make([]string, len(args))
		for i, a := range args {
			terms[i] = a.term
			if k == ir.Decimal && a.kind != ir.Decimal {
				terms[i] = toReal(a)
			}
		}
		switch e.Fn {
		case "min":
			return typed{term: fmt.Sprintf("(ite (<= %s %s) %s %s)", terms[0], terms[1], terms[0], terms[1]), kind: k}
		case "max":
			return typed{term: fmt.Sprintf("(ite (>= %s %s) %s %s)", terms[0], terms[1], terms[0], terms[1]), kind: k}
		}
		if k == ir.Time {
			return lw.fail(where, "%s: abs takes a number or a duration", e)
		}
		zero := "0"
		if k == ir.Decimal {
			zero = "0.0"
		}
		return typed{term: fmt.Sprintf("(ite (< %s %s) (- %s) %s)", terms[0], zero, terms[0], terms[0]), kind: k}
	case "isSet":
		if args[0].set == "" {
			return lw.fail(where, "%s: isSet takes an optional attribute (declare it with optional: true)", e)
		}
		return typed{term: args[0].set, kind: ir.Bool}
	case "has":
		m := args[0]
		if m.kind != ir.Map || m.keys == "" {
			return lw.fail(where, "%s: has takes a map attribute", e)
		}
		k, ok := lw.as(args[1], m.t.Key, e, where)
		if !ok {
			return typed{term: "false", kind: ir.Bool}
		}
		return typed{term: fmt.Sprintf("(select %s %s)", m.keys, k), kind: ir.Bool}
	}
	return lw.fail(where, "%s: unknown function %s", e, e.Fn)
}

// members are the values of a finite element type, as typed terms.
func (lw *lowerer) members(t *ir.Attr) []typed {
	if t.Kind == ir.Bool {
		return []typed{{term: "false", kind: ir.Bool, t: t}, {term: "true", kind: ir.Bool, t: t}}
	}
	var out []typed
	for _, v := range t.Values {
		out = append(out, typed{term: lw.enumValue(t, v, nil, ""), kind: ir.Enum, t: t})
	}
	return out
}

func sum(terms []string) string {
	switch len(terms) {
	case 0:
		return "0"
	case 1:
		return terms[0]
	}
	return "(+ " + strings.Join(terms, " ") + ")"
}

// quant expands a quantifier over a bounded collection: the slots of a
// relation to many, a list's positions up to its maxLength, or the values a
// set of enum or bool values may hold.
func (lw *lowerer) quant(e *ir.Quant, subject *ir.Entity, where string) typed {
	c := lw.expr(e.Coll, subject, where)
	type item struct {
		in string
		b  binding
	}
	var items []item
	switch {
	case c.coll != nil:
		rel, from := c.coll.rel, c.coll.from
		to := lw.reg.Entities[rel.To]
		owner := from
		for i := 0; i < rel.Max; i++ {
			prefix := fmt.Sprintf("%s.%s#%d", from.prefix, rel.Name, i+1)
			in := lw.flagVar(prefix + ":in")
			items = append(items, item{in: in.Name, b: binding{name: e.Var, at: &place{entity: to, prefix: prefix, slot: true, via: rel, owner: &owner}}})
		}
	case c.kind == ir.List && c.t != nil:
		// An unset optional collection has no elements.
		for i, el := range c.elems {
			v := typed{term: el, kind: c.t.Elem.Kind, t: c.t.Elem}
			items = append(items, item{in: guard(c.set, fmt.Sprintf("(< %d %s)", i, c.length)), b: binding{name: e.Var, val: &v}})
		}
	case c.kind == ir.SetKind && c.t != nil && c.t.Bounded():
		// Count each element once: a position counts if no earlier one repeats it.
		for i, el := range c.elems {
			v := typed{term: el, kind: c.t.Elem.Kind, t: c.t.Elem}
			in := fmt.Sprintf("(< %d %s)", i, c.length)
			if e.Op == "count" {
				for j := 0; j < i; j++ {
					in = fmt.Sprintf("(and %s (not (= %s %s)))", in, c.elems[j], el)
				}
			}
			items = append(items, item{in: guard(c.set, in), b: binding{name: e.Var, val: &v}})
		}
	case c.kind == ir.SetKind && c.t != nil && (c.t.Elem.Kind == ir.Enum || c.t.Elem.Kind == ir.Bool):
		for _, m := range lw.members(c.t.Elem) {
			v := m
			items = append(items, item{in: guard(c.set, fmt.Sprintf("(select %s %s)", c.term, m.term)), b: binding{name: e.Var, val: &v}})
		}
	default:
		return lw.fail(where, "%s: %s ranges over a relation to many, a list, or a set of enum or bool values", e, e.Op)
	}
	var terms []string
	for _, it := range items {
		lw.scope = append(lw.scope, it.b)
		body := lw.expr(e.Body, subject, where)
		lw.scope = lw.scope[:len(lw.scope)-1]
		if body.kind != ir.Bool || body.lit != "" || body.coll != nil {
			return lw.fail(where, "%s: the body is not a condition", e)
		}
		switch e.Op {
		case "all":
			terms = append(terms, fmt.Sprintf("(=> %s %s)", it.in, body.term))
		case "any":
			terms = append(terms, fmt.Sprintf("(and %s %s)", it.in, body.term))
		case "count":
			terms = append(terms, fmt.Sprintf("(ite (and %s %s) 1 0)", it.in, body.term))
		}
	}
	switch e.Op {
	case "all":
		return typed{term: conj(terms), kind: ir.Bool}
	case "any":
		if len(terms) == 0 {
			return typed{term: "false", kind: ir.Bool}
		}
		if len(terms) == 1 {
			return typed{term: terms[0], kind: ir.Bool}
		}
		return typed{term: "(or " + strings.Join(terms, " ") + ")", kind: ir.Bool}
	}
	return typed{term: sum(terms), kind: ir.Int}
}

func (lw *lowerer) binary(e *ir.Binary, subject *ir.Entity, where string) typed {
	switch e.Op {
	case "and", "or", "implies":
		op := map[string]string{"and": "and", "or": "or", "implies": "=>"}[e.Op]
		l, r := lw.boolTerm(e.L, subject, where), lw.boolTerm(e.R, subject, where)
		return typed{term: fmt.Sprintf("(%s %s %s)", op, l, r), kind: ir.Bool}
	case "in":
		set, ok := e.R.(*ir.Set)
		if !ok {
			return lw.fail(where, "%s: `in` needs a set literal on the right (for a set attribute, use contains)", e)
		}
		l := lw.expr(e.L, subject, where)
		var eqs []string
		for _, el := range set.Elems {
			eqs = append(eqs, lw.equal(l, lw.expr(el, subject, where), e, where))
		}
		if len(eqs) == 1 {
			return typed{term: eqs[0], kind: ir.Bool}
		}
		return typed{term: "(or " + strings.Join(eqs, " ") + ")", kind: ir.Bool}
	case "=", "!=":
		l, r := lw.expr(e.L, subject, where), lw.expr(e.R, subject, where)
		eq := lw.equal(l, r, e, where)
		if unchanged(e.L, e.R) && (l.set != "" || r.set != "") {
			// x = old(x) says x is unchanged: unset before and after
			// counts, though an unset value equals nothing otherwise
			// (D-052). Read as equality, "a refused lend changes
			// nothing" was impossible whenever the borrower was unset.
			eq = fmt.Sprintf("(or %s (and (not %s) (not %s)))", eq, orTrue(l.set), orTrue(r.set))
		}
		if e.Op == "!=" {
			eq = "(not " + eq + ")"
		}
		return typed{term: eq, kind: ir.Bool}
	case "<", "<=", ">", ">=":
		lt, rt := lw.expr(e.L, subject, where), lw.expr(e.R, subject, where)
		l, r, k, ok := lw.numbers(e, lt, rt, where)
		if !ok {
			return typed{term: "false", kind: ir.Bool}
		}
		if k == ir.String {
			return lw.fail(where, "%s: strings are compared with =, startsWith, endsWith and contains", e)
		}
		return typed{term: guard(presence(lt, rt), fmt.Sprintf("(%s %s %s)", e.Op, l, r)), kind: ir.Bool}
	case "+", "-":
		lt, rt := lw.expr(e.L, subject, where), lw.expr(e.R, subject, where)
		set := presence(lt, rt)
		if e.Op == "+" && lt.kind == ir.String && rt.kind == ir.String && lt.coll == nil && rt.coll == nil {
			return typed{term: fmt.Sprintf("(str.++ %s %s)", lt.term, rt.term), kind: ir.String, set: set}
		}
		// Times and durations: a time plus or minus a duration is a time; the
		// difference of two times is a duration.
		switch {
		case lt.kind == ir.Time && rt.kind == ir.Duration:
			return typed{term: fmt.Sprintf("(%s %s %s)", e.Op, lt.term, rt.term), kind: ir.Time, set: set}
		case e.Op == "+" && lt.kind == ir.Duration && rt.kind == ir.Time:
			return typed{term: fmt.Sprintf("(+ %s %s)", lt.term, rt.term), kind: ir.Time, set: set}
		case e.Op == "-" && lt.kind == ir.Time && rt.kind == ir.Time:
			return typed{term: fmt.Sprintf("(- %s %s)", lt.term, rt.term), kind: ir.Duration, set: set}
		}
		l, r, k, ok := lw.numbers(e, lt, rt, where)
		if !ok {
			return typed{term: "0", kind: ir.Int}
		}
		if k == ir.Time || k == ir.String {
			return lw.fail(where, "%s: `%s` does not apply to two %ss", e, e.Op, ir.KindNames[k])
		}
		return typed{term: fmt.Sprintf("(%s %s %s)", e.Op, l, r), kind: k, set: set}
	}
	return lw.fail(where, "unsupported operator %s", e.Op)
}

// numbers types the operands of an ordering or arithmetic operator: ints and
// decimals mix (as decimals); durations, times and strings only with their
// own kind.
func (lw *lowerer) numbers(e ir.Expr, l, r typed, where string) (string, string, ir.Kind, bool) {
	if l.coll != nil || r.coll != nil || l.lit != "" || r.lit != "" || l.none || r.none {
		lw.fail(where, "%s: `%s` needs numbers on both sides", e, opOf(e))
		return "", "", 0, false
	}
	switch {
	case l.kind == r.kind && (l.kind.Numeric() || l.kind == ir.String):
		return l.term, r.term, l.kind, true
	case l.kind == ir.Decimal && r.kind == ir.Int:
		return l.term, toReal(r), ir.Decimal, true
	case l.kind == ir.Int && r.kind == ir.Decimal:
		return toReal(l), r.term, ir.Decimal, true
	case (l.kind == ir.Duration && r.numLit) || (r.kind == ir.Duration && l.numLit):
		lw.fail(where, "%s: write durations with a unit, such as 500ms, 30s, 15m, 2h or 7d", e)
		return "", "", 0, false
	case (l.kind == ir.Time && r.numLit) || (r.kind == ir.Time && l.numLit):
		lw.fail(where, "%s: write times as dates, such as 2025-01-01 or 2025-01-01T10:30:00Z", e)
		return "", "", 0, false
	}
	lw.fail(where, "%s: `%s` needs ints on both sides, or numbers of one kind (%s and %s)", e, opOf(e), ir.KindNames[l.kind], ir.KindNames[r.kind])
	return "", "", 0, false
}

func opOf(e ir.Expr) string {
	if b, ok := e.(*ir.Binary); ok {
		return b.Op
	}
	return "?"
}

// equal lowers l = r, giving bare enum literals the type of the other side.
func (lw *lowerer) equal(l, r typed, e ir.Expr, where string) string {
	if l.none || r.none {
		return lw.equalRaw(l, r, e, where)
	}
	return guard(presence(l, r), lw.equalRaw(l, r, e, where))
}

func (lw *lowerer) equalRaw(l, r typed, e ir.Expr, where string) string {
	if l.lit != "" && r.lit != "" {
		lw.fail(where, "%s: cannot compare two enum literals", e)
		return "false"
	}
	if l.none || r.none {
		if l.none {
			l, r = r, l
		}
		if r.none && l.none {
			return "true"
		}
		if l.set == "" {
			lw.fail(where, "%s: only an optional attribute can be none (declare it with optional: true)", e)
			return "false"
		}
		return "(not " + l.set + ")"
	}
	if l.coll != nil || r.coll != nil {
		lw.fail(where, "%s: a relation to many is ranged over with all, any or count, not compared", e)
		return "false"
	}
	if r.items != nil && l.items == nil {
		l, r = r, l
	}
	if l.items != nil {
		// A list literal takes the type of the collection it is compared with.
		if r.items != nil || r.t == nil || (r.kind != ir.List && r.kind != ir.SetKind) {
			lw.fail(where, "%s: a list literal compares with a list or set attribute", e)
			return "false"
		}
		var elems []string
		for _, x := range l.items {
			v, ok := lw.as(x, r.t.Elem, e, where)
			if !ok {
				return "false"
			}
			elems = append(elems, v)
		}
		if r.kind == ir.SetKind && !r.t.Bounded() {
			term := fmt.Sprintf("((as const %s) false)", lw.sortOf(r.t))
			for _, v := range elems {
				term = fmt.Sprintf("(store %s %s true)", term, v)
			}
			return fmt.Sprintf("(= %s %s)", r.term, term)
		}
		lit := typed{length: strconv.Itoa(len(elems)), elems: elems}
		if r.kind == ir.SetKind {
			return sameSet(r, lit)
		}
		if len(elems) > len(r.elems) {
			return "false" // longer than the list is checked
		}
		return sameList(r, lit)
	}
	if slottedTyped(l) && slottedTyped(r) {
		if l.kind == ir.SetKind {
			return sameSet(l, r)
		}
		return sameList(l, r)
	}
	if l.kind == kindRelation || r.kind == kindRelation {
		lw.fail(where, "%s: an optional relation compares only with none (or use isSet)", e)
		return "false"
	}
	if l.lit != "" {
		l, r = r, l
	}
	if r.lit != "" {
		if l.enum() == nil {
			lw.fail(where, "%s: %q is not a value here (is it a path missing its root?)", e, r.lit)
			return "false"
		}
		return fmt.Sprintf("(= %s %s)", l.term, lw.enumValue(l.enum(), r.lit, e, where))
	}
	if l.kind == ir.Decimal && r.kind == ir.Int {
		return fmt.Sprintf("(= %s %s)", l.term, toReal(r))
	}
	if l.kind == ir.Int && r.kind == ir.Decimal {
		return fmt.Sprintf("(= %s %s)", toReal(l), r.term)
	}
	if l.kind != r.kind {
		lw.fail(where, "%s: operands have different types", e)
		return "false"
	}
	if le, re := l.enum(), r.enum(); le != nil && re != nil && le != re {
		if !sameValues(le, re) {
			lw.fail(where, "%s: %s and %s have incompatible enum domains; compare each to a value", e, le.QName(), re.QName())
			return "false"
		}
		// Each attribute has its own SMT datatype. Equality of compatible
		// enums means equality of their declared value names, never equality
		// between constructors of different sorts or their ordinal positions.
		left := slices.Clone(le.Values)
		slices.Sort(left)
		var cases []string
		for _, value := range left {
			cases = append(cases, fmt.Sprintf("(and (= %s %s) (= %s %s))", l.term, EnumConst(le, value), r.term, EnumConst(re, value)))
		}
		return "(or " + strings.Join(cases, " ") + ")"
	}
	if l.t != nil && r.t != nil && (l.kind == ir.SetKind || l.kind == ir.Map) && lw.sortOf(l.t) != lw.sortOf(r.t) {
		lw.fail(where, "%s: %s and %s are collections of different types", e, l.t.QName(), r.t.QName())
		return "false"
	}
	return fmt.Sprintf("(= %s %s)", l.term, r.term)
}

func slottedTyped(t typed) bool { return t.t != nil && slotted(t.t) && t.length != "" }

// holds says a slotted collection holds value v at some position.
func holds(c typed, v string) string {
	var parts []string
	for i, el := range c.elems {
		parts = append(parts, fmt.Sprintf("(and (< %d %s) (= %s %s))", i, c.length, el, v))
	}
	if len(parts) == 0 {
		return "false"
	}
	return "(or " + strings.Join(parts, " ") + ")"
}

// sameList says two slotted lists have the same length and elements.
func sameList(a, b typed) string {
	n := min(len(a.elems), len(b.elems))
	parts := []string{fmt.Sprintf("(= %s %s)", a.length, b.length), fmt.Sprintf("(<= %s %d)", a.length, n)}
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("(=> (< %d %s) (= %s %s))", i, a.length, a.elems[i], b.elems[i]))
	}
	return conj(parts)
}

// sameSet says two slotted collections hold the same elements, in any order
// and whatever repeats.
func sameSet(a, b typed) string {
	var parts []string
	for i, el := range a.elems {
		parts = append(parts, fmt.Sprintf("(=> (< %d %s) %s)", i, a.length, holds(b, el)))
	}
	for i, el := range b.elems {
		parts = append(parts, fmt.Sprintf("(=> (< %d %s) %s)", i, b.length, holds(a, el)))
	}
	return conj(parts)
}

func (lw *lowerer) enumValue(a *ir.Attr, v string, e ir.Expr, where string) string {
	for _, x := range a.Values {
		if x == v {
			lw.sortOf(a)
			return EnumConst(a, v)
		}
	}
	lw.fail(where, "%s: %q is not a value of %s (values: %s)", e, v, a.QName(), strings.Join(a.Values, ", "))
	return "false"
}

// literal lowers a literal value for attribute a (a default or an effect).
func (lw *lowerer) literal(e ir.Expr, a *ir.Attr, where string) string {
	switch e := e.(type) {
	case *ir.IntLit:
		switch a.Kind {
		case ir.Int:
			if a.HasRange && (e.V < a.Min || e.V > a.Max) {
				lw.fail(where, "%d is outside %s's range [%d, %d]", e.V, a.QName(), a.Min, a.Max)
			}
			return smtInt(e.V)
		case ir.Decimal:
			return smtReal(strconv.Itoa(e.V))
		}
	case *ir.DecLit:
		if a.Kind == ir.Decimal {
			return smtReal(e.Text)
		}
	case *ir.StrLit:
		if a.Kind == ir.String {
			return smtString(e.V)
		}
	case *ir.DurLit:
		if a.Kind == ir.Duration {
			return smtInt(e.MS)
		}
	case *ir.TimeLit:
		if a.Kind == ir.Time {
			return smtInt(e.MS)
		}
	case *ir.BoolLit:
		if a.Kind == ir.Bool {
			return strconv.FormatBool(e.V)
		}
	case *ir.Ident:
		if a.Kind == ir.Enum {
			return lw.enumValue(a, e.Name, e, where)
		}
	}
	lw.fail(where, "%s does not fit %s", e, a.QName())
	return "false"
}

// regexTerm translates a regular expression to an SMT-LIB regular language.
// The whole string must match. Supported: literals, ., character classes,
// *, +, ?, {m,n}, | and groups; anchors are implied.
func regexTerm(pattern string) (string, error) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", fmt.Errorf("bad regular expression: %v", err)
	}
	var tr func(r *syntax.Regexp) (string, error)
	all := func(subs []*syntax.Regexp, op string, empty string) (string, error) {
		var parts []string
		for _, s := range subs {
			t, err := tr(s)
			if err != nil {
				return "", err
			}
			if t != "" {
				parts = append(parts, t)
			}
		}
		switch len(parts) {
		case 0:
			return empty, nil
		case 1:
			return parts[0], nil
		}
		return "(" + op + " " + strings.Join(parts, " ") + ")", nil
	}
	tr = func(r *syntax.Regexp) (string, error) {
		switch r.Op {
		case syntax.OpLiteral:
			return "(str.to_re " + smtString(string(r.Rune)) + ")", nil
		case syntax.OpCharClass:
			var parts []string
			for i := 0; i+1 < len(r.Rune); i += 2 {
				parts = append(parts, fmt.Sprintf("(re.range %s %s)", smtString(string(r.Rune[i])), smtString(string(r.Rune[i+1]))))
			}
			if len(parts) == 1 {
				return parts[0], nil
			}
			return "(re.union " + strings.Join(parts, " ") + ")", nil
		case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
			return "re.allchar", nil
		case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText:
			return "", nil
		case syntax.OpCapture:
			return tr(r.Sub[0])
		case syntax.OpStar, syntax.OpPlus, syntax.OpQuest:
			t, err := tr(r.Sub[0])
			if err != nil {
				return "", err
			}
			if t == "" {
				t = `(str.to_re "")`
			}
			op := map[syntax.Op]string{syntax.OpStar: "re.*", syntax.OpPlus: "re.+", syntax.OpQuest: "re.opt"}[r.Op]
			return "(" + op + " " + t + ")", nil
		case syntax.OpRepeat:
			t, err := tr(r.Sub[0])
			if err != nil {
				return "", err
			}
			if r.Max < 0 {
				return fmt.Sprintf("(re.++ ((_ re.loop %d %d) %s) (re.* %s))", r.Min, r.Min, t, t), nil
			}
			return fmt.Sprintf("((_ re.loop %d %d) %s)", r.Min, r.Max, t), nil
		case syntax.OpConcat:
			return all(r.Sub, "re.++", `(str.to_re "")`)
		case syntax.OpAlternate:
			return all(r.Sub, "re.union", "re.none")
		}
		return "", fmt.Errorf("regular expression feature %v is not supported", r.Op)
	}
	t, err := tr(re.Simplify())
	if t == "" && err == nil {
		t = `(str.to_re "")`
	}
	return t, err
}

// numericKind is the kind min, max and abs return for args: one kind of
// number, duration or time; whole numbers and decimals mix as decimals, and
// a plain number literal takes the other argument's kind.
func numericKind(args []typed) (ir.Kind, bool) {
	k := ir.Kind(-1)
	for _, a := range args {
		switch a.kind {
		case ir.Int, ir.Decimal, ir.Duration, ir.Time:
		default:
			return k, false
		}
		switch {
		case k == ir.Kind(-1), a.kind == k:
			k = a.kind
		case a.numLit:
		case (k == ir.Int && a.kind == ir.Decimal) || (k == ir.Decimal && a.kind == ir.Int):
			k = ir.Decimal
		default:
			if k == ir.Int && args[0].numLit {
				k = a.kind // the literal takes the other's kind
				continue
			}
			return k, false
		}
	}
	return k, true
}

// unchanged reports whether one side of a comparison is old() of the
// other: the idiom for a value a step keeps.
func unchanged(a, b ir.Expr) bool {
	isOld := func(c, x ir.Expr) bool {
		call, ok := c.(*ir.Call)
		return ok && call.Fn == "old" && len(call.Args) == 1 && call.Args[0].String() == x.String()
	}
	return isOld(a, b) || isOld(b, a)
}

func orTrue(set string) string {
	if set == "" {
		return "true"
	}
	return set
}

// forTarget is reg.ForTarget for the current step, made once per rule.
func (lw *lowerer) forTarget(x *ir.Rule) *ir.Rule {
	if lw.op == nil {
		return x
	}
	k := [2]string{lw.op.Name, x.ID}
	if r, ok := lw.targeted[k]; ok {
		return r
	}
	tie, ok := lw.targets[lw.op.Name]
	if !ok {
		tie = lw.reg.Target(lw.op.Name)
		lw.targets[lw.op.Name] = tie
	}
	r := lw.reg.ForTarget(lw.op.Name, tie, x)
	lw.targeted[k] = r
	return r
}

// forOtherInstances is reg.ForOtherInstances, made once per rule.
func (lw *lowerer) forOtherInstances(x *ir.Rule) *ir.Rule {
	r, ok := lw.lifted[x.ID]
	if !ok {
		r = lw.reg.ForOtherInstances(x)
		lw.lifted[x.ID] = r
	}
	return r
}
