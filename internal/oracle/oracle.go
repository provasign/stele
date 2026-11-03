// Package oracle decides the checker's questions by brute force, as an
// independent reference for testing it. It interprets the IR directly and
// enumerates every state, request and outcome, sharing nothing with the SMT
// lowering except path resolution. It is exponential and meant only for the
// small registries property tests generate.
//
// The semantics are those of D-042 (docs/05#situation-and-response):
//   - the state unit asks whether every situation (attributes no operation
//     writes) has a state satisfying the state invariants;
//   - each operation's unit asks whether, from every state satisfying all
//     state invariants and every request, the operation can be handled so
//     that every rule about it holds and every state invariant holds after;
//   - an operation's default applies when none of its allow (deny) rules
//     match, and it does not run unless its preconditions hold;
//   - attributes an operation does not write keep their value, and an
//     operation that does not run changes nothing;
//   - a step's result (D-064) is part of its response, with no frame;
//   - decisions and assumptions are as in the SMT lowering's package doc.
//
// Unlike the lowering, the oracle never slices: it checks every invariant
// before and after every step, so it also tests that slicing is sound.
package oracle

import (
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/provasign/stele/internal/ir"
)

// Value is one variable's value: a bool, an int (also durations and times,
// in milliseconds), an enum value name, a decimal (as an exact rational), a
// string, or a list, set or map (a key into Model.comp, which holds its
// elements). Values compare with ==.
type Value struct {
	B bool
	I int
	E string
	D string
	S string
	C string
}

// Format renders a value of variable v the way the checker prints it.
func (v *Var) Format(val Value) string {
	if v.Attr == nil || v.Part != "" {
		if v.Part == "#keys" {
			return val.C
		}
		return fmt.Sprint(val.B)
	}
	switch v.Attr.Kind {
	case ir.Bool:
		return fmt.Sprint(val.B)
	case ir.Int, ir.Duration, ir.Time:
		return fmt.Sprint(val.I)
	case ir.Decimal:
		return val.D
	case ir.String:
		return `"` + strings.ReplaceAll(val.S, `"`, `""`) + `"`
	case ir.List, ir.SetKind, ir.Map:
		return val.C
	}
	return val.E
}

// Var is one variable: an attribute before a step (or in the state unit), an
// attribute after a step (Post), an operation's runs, or a request attribute.
type Var struct {
	Name   string // Workspace.status, Workspace.status', Workspace.stop:runs
	Attr   *ir.Attr
	Post   bool
	Domain []Value
	// Wide is the domain on the response side, when it differs: a sampled
	// number also takes the values one literal step away from the sample, so
	// a response such as "one more run" or "an hour before now" exists for
	// every situation value.
	Wide []Value
	// Prefix is the instance (an entity name, or a slot such as
	// Node.workspaces#1); Part is "" for the value, "?" for an optional
	// value's presence and "#keys" for a map's keys.
	Prefix, Part string
}

type evalFn func([]Value) Value
type pred func([]Value) bool

// Rule is a rule as one unit evaluates it.
type Rule struct {
	ID    string
	Reads []string // variable names, sorted
	eval  pred
}

// Unit is one question, as in the lowering.
type Unit struct {
	Name      string
	Situation []int // variable indices enumerated as the situation
	Response  []int // variable indices enumerated as the response
	Rules     []*Rule
	assumed   []pred
	assumedID []string // the rule each of assumed comes from, in a step unit
	fixed     []pred
	cands     []Response
	// possible are permissions with effect possible: some allowed situation
	// and response must satisfy them with the rules.
	possible []*Rule
}

// Response is a candidate dead response: an enum value or an operation running.
type Response struct {
	Var   int
	Value Value
	Label string // as the checker names it
}

// Model is a registry prepared for enumeration.
type Model struct {
	Vars  []*Var
	Units []*Unit
	index map[string]int
	reg   *ir.Registry
	// comp holds the elements of every list, set and map value; a map's are
	// key, value pairs in key order.
	comp map[string][]Value
	// Literals in the registry, from which infinite domains are sampled.
	strs []string
	ints []int
	decs []*big.Rat
}

// MaxStates bounds enumeration of either side of a unit.
const MaxStates = 1 << 15

func (m *Model) varFor(a *ir.Attr, post bool) int { return m.varAt(a.Entity.Name, a, "", post) }

// varAt is the variable for part of attribute a of the instance at prefix,
// named as the lowering names it.
func (m *Model) varAt(prefix string, a *ir.Attr, part string, post bool) int {
	name := prefix + "." + a.Name + part
	if post {
		name += "'"
	}
	if i, ok := m.index[name]; ok {
		return i
	}
	v := &Var{Name: name, Attr: a, Post: post, Prefix: prefix, Part: part}
	switch part {
	case "":
		v.Domain = m.domain(a)
		v.Wide = m.widen(a, v.Domain)
	case "?":
		v.Domain = []Value{{B: false}, {B: true}}
	case "#keys":
		v.Domain = m.subsets(m.domain(a.Key))
	}
	m.Vars = append(m.Vars, v)
	m.index[name] = len(m.Vars) - 1
	return m.index[name]
}

// flag is a Bool of the situation: a slot's membership.
func (m *Model) flag(name string) int {
	if i, ok := m.index[name]; ok {
		return i
	}
	m.Vars = append(m.Vars, &Var{Name: name, Domain: []Value{{B: false}, {B: true}}})
	m.index[name] = len(m.Vars) - 1
	return m.index[name]
}

// domain enumerates the values of type a. Finite types are enumerated
// exactly. Strings, decimals and unbounded numbers range over the registry's
// literals and values around and between them, so the oracle is complete only
// for registries whose questions those values decide (as the tests' are).
func (m *Model) domain(a *ir.Attr) []Value {
	var out []Value
	switch a.Kind {
	case ir.Bool:
		out = []Value{{B: false}, {B: true}}
	case ir.Enum:
		for _, e := range a.Values {
			out = append(out, Value{E: e})
		}
	case ir.Int, ir.Duration, ir.Time:
		if a.Kind == ir.Int && a.HasRange && a.Max-a.Min <= 64 {
			for i := a.Min; i <= a.Max; i++ {
				out = append(out, Value{I: i})
			}
			break
		}
		// A wide range is sampled like an unbounded number, within it.
		seen := map[int]bool{}
		bases := []int{0, 1, 2}
		for _, n := range m.ints {
			bases = append(bases, n, -n) // differences of two values reach either sign
		}
		if a.HasRange {
			bases = append(bases, a.Min, a.Max)
		}
		for _, n := range bases {
			for _, x := range []int{n - 1, n, n + 1} {
				if !seen[x] && (!a.HasRange || (x >= a.Min && x <= a.Max)) {
					seen[x] = true
					out = append(out, Value{I: x})
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].I < out[j].I })
	case ir.Decimal:
		pts := []*big.Rat{new(big.Rat)}
		pts = append(pts, m.decs...)
		for _, n := range m.ints {
			pts = append(pts, new(big.Rat).SetInt64(int64(n)))
		}
		var lo, hi *big.Rat
		if a.HasRange {
			lo, _ = new(big.Rat).SetString(a.RMin)
			hi, _ = new(big.Rat).SetString(a.RMax)
			pts = append(pts, lo, hi)
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].Cmp(pts[j]) < 0 })
		var all []*big.Rat
		for i, p := range pts {
			if i > 0 {
				all = append(all, new(big.Rat).Quo(new(big.Rat).Add(pts[i-1], p), big.NewRat(2, 1)))
			}
			all = append(all, p)
		}
		all = append(all, new(big.Rat).Sub(pts[0], big.NewRat(1, 1)), new(big.Rat).Add(pts[len(pts)-1], big.NewRat(1, 1)))
		seen := map[string]bool{}
		for _, r := range all {
			if a.HasRange && (r.Cmp(lo) < 0 || r.Cmp(hi) > 0) {
				continue
			}
			if v := decimal(r); !seen[v.D] {
				seen[v.D] = true
				out = append(out, v)
			}
		}
	case ir.String:
		seen := map[string]bool{}
		add := func(s string) {
			if !seen[s] {
				seen[s] = true
				out = append(out, Value{S: s})
			}
		}
		add("")
		add("z")
		for _, x := range m.strs {
			add(x)
		}
		for _, x := range m.strs {
			for _, y := range m.strs {
				add(x + y)
			}
		}
	case ir.List:
		elems := m.domain(a.Elem)
		var rec func(prefix []Value)
		rec = func(prefix []Value) {
			out = append(out, m.compose("L", prefix))
			if len(prefix) < a.MaxLen {
				for _, e := range elems {
					rec(append(slices.Clone(prefix), e))
				}
			}
		}
		rec(nil)
	case ir.SetKind:
		if a.Bounded() { // up to maxLength elements, as the checker bounds them
			out = m.subsetsUpTo(m.domain(a.Elem), a.MaxLen)
		} else {
			out = m.subsets(m.domain(a.Elem))
		}
	case ir.Map:
		keys, vals := m.domain(a.Key), m.domain(a.Elem)
		var rec func(k int, pairs []Value)
		rec = func(k int, pairs []Value) {
			if k == len(keys) {
				out = append(out, m.compose("M", pairs))
				return
			}
			for _, v := range vals {
				rec(k+1, append(slices.Clone(pairs), keys[k], v))
			}
		}
		rec(0, nil)
	}
	return out
}

// widen returns the response-side domain of a sampled number type, or nil.
func (m *Model) widen(a *ir.Attr, dom []Value) []Value {
	sampled := a.Kind == ir.Duration || a.Kind == ir.Time || (a.Kind == ir.Int && !(a.HasRange && a.Max-a.Min <= 64))
	if !sampled {
		return nil
	}
	steps := []int{1}
	for _, n := range m.ints {
		steps = append(steps, n)
	}
	seen := map[int]bool{}
	var out []Value
	for _, d := range dom {
		for _, k := range steps {
			for _, x := range []int{d.I, d.I - k, d.I + k} {
				if !seen[x] && (!a.HasRange || (x >= a.Min && x <= a.Max)) {
					seen[x] = true
					out = append(out, Value{I: x})
				}
			}
		}
	}
	return out
}

// subsetsUpTo enumerates the subsets of at most n elements.
func (m *Model) subsetsUpTo(elems []Value, n int) []Value {
	var out []Value
	var rec func(start int, in []Value)
	rec = func(start int, in []Value) {
		out = append(out, m.compose("S", in))
		if len(in) == n {
			return
		}
		for i := start; i < len(elems); i++ {
			rec(i+1, append(slices.Clone(in), elems[i]))
		}
	}
	rec(0, nil)
	return out
}

func (m *Model) subsets(elems []Value) []Value {
	var out []Value
	for mask := 0; mask < 1<<len(elems); mask++ {
		var in []Value
		for i, e := range elems {
			if mask&(1<<i) != 0 {
				in = append(in, e)
			}
		}
		out = append(out, m.compose("S", in))
	}
	return out
}

// compose records a list, set or map value and returns it; equal contents
// give equal values.
func (m *Model) compose(kind string, elems []Value) Value {
	if kind == "S" { // a set: its elements in one order
		elems = slices.Clone(elems)
		sort.Slice(elems, func(i, j int) bool { return enc(elems[i]) < enc(elems[j]) })
	}
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = strconv.Quote(enc(e))
	}
	key := kind + "[" + strings.Join(parts, ",") + "]"
	m.comp[key] = elems
	return Value{C: key}
}

func enc(v Value) string { return fmt.Sprintf("%v|%d|%s|%s|%s|%s", v.B, v.I, v.E, v.D, v.S, v.C) }

// literals collects the registry's string and number literals.
func (m *Model) literals(reg *ir.Registry) {
	seenS, seenI, seenD := map[string]bool{}, map[int]bool{}, map[string]bool{}
	var walk func(e ir.Expr)
	walk = func(e ir.Expr) {
		switch e := e.(type) {
		case *ir.StrLit:
			if !seenS[e.V] {
				seenS[e.V] = true
				m.strs = append(m.strs, e.V)
			}
		case *ir.IntLit:
			if !seenI[e.V] {
				seenI[e.V] = true
				m.ints = append(m.ints, e.V)
			}
		case *ir.DurLit:
			walk(&ir.IntLit{V: e.MS})
		case *ir.TimeLit:
			walk(&ir.IntLit{V: e.MS})
		case *ir.DecLit:
			if r, ok := new(big.Rat).SetString(e.Text); ok && !seenD[r.RatString()] {
				seenD[r.RatString()] = true
				m.decs = append(m.decs, r)
			}
		case *ir.Binary:
			walk(e.L)
			walk(e.R)
		case *ir.Not:
			walk(e.X)
		case *ir.Scale:
			walk(e.X)
		case *ir.Set:
			for _, x := range e.Elems {
				walk(x)
			}
		case *ir.ListLit:
			for _, x := range e.Elems {
				walk(x)
			}
		case *ir.Call:
			for _, x := range e.Args {
				walk(x)
			}
		case *ir.Index:
			walk(e.X)
			walk(e.K)
		case *ir.Quant:
			walk(e.Coll)
			walk(e.Body)
		}
	}
	for _, x := range reg.Rules {
		for _, e := range []ir.Expr{x.When, x.Assert} {
			if e != nil {
				walk(e)
			}
		}
	}
	for _, op := range reg.Operations {
		for _, as := range op.Effect {
			walk(as.Value)
		}
	}
	sort.Strings(m.strs)
	sort.Ints(m.ints)
}

func (m *Model) runsVar(op string) int {
	name := op + ":runs"
	if i, ok := m.index[name]; ok {
		return i
	}
	m.Vars = append(m.Vars, &Var{Name: name, Domain: []Value{{B: false}, {B: true}}})
	m.index[name] = len(m.Vars) - 1
	return m.index[name]
}

// ctx is how paths resolve while compiling one rule in one unit.
type ctx struct {
	op    *ir.Operation // nil in the state unit
	after bool          // reading the state after the step
	reads map[int]bool
	// asserted, when set, collects what invariant asserts read: the
	// response side of the assumptions unit.
	asserted map[int]bool
	scope    []obind // quantifier variables, innermost last
}

// Build prepares reg. withDecisions applies decision records.
func Build(reg *ir.Registry, withDecisions bool) (*Model, error) {
	return BuildWith(reg, withDecisions, false)
}

// BuildWith is Build, and with assumeDefaults also assumes every configured
// default holds, like lower.Options.AssumeDefaults.
func BuildWith(reg *ir.Registry, withDecisions, assumeDefaults bool) (*Model, error) {
	m := &Model{index: map[string]int{}, reg: reg, comp: map[string][]Value{}}
	m.literals(reg)
	retired := map[string]bool{}
	superseded := map[string][]*ir.Decision{}
	if withDecisions {
		for _, d := range reg.Decisions {
			for _, p := range d.Retire {
				retired[p.Rule] = true
			}
			if d.Supersede != nil {
				superseded[d.Supersede.Loser] = append(superseded[d.Supersede.Loser], d)
			}
		}
	}
	var live []*ir.Rule
	for _, x := range reg.Rules {
		if x.Live() && !retired[x.ID] && x.Kind != ir.Measured && x.Kind != ir.Informal && x.Kind != ir.Obligation && x.Kind != ir.Derivation {
			live = append(live, x)
		}
	}

	compileRule := func(x *ir.Rule, c *ctx) (pred, error) {
		subject, err := reg.SubjectOf(x)
		if err != nil {
			return nil, err
		}
		f, err := m.ruleFn(x, subject, c)
		if err != nil {
			return nil, err
		}
		for _, d := range superseded[x.ID] {
			cond, err := m.boolFn(d.Supersede.When, nil, c)
			if err != nil {
				return nil, err
			}
			inner := f
			f = func(s []Value) bool { return cond(s) || inner(s) }
		}
		// A rule whose text overrides x's statement, where it applies here.
		state := func(r *ir.Rule) bool { return r.Kind == ir.Invariant && r.On == "" }
		for _, w := range reg.Overriders(x.Statement()) {
			if w == x || w.Statement() == x.Statement() {
				continue
			}
			same := (c.op == nil && state(w) && state(x)) ||
				(c.op != nil && w.AppliesTo(c.op.Name) && x.AppliesTo(c.op.Name)) ||
				(c.op != nil && state(w) && state(x))
			if !same {
				continue
			}
			ws, err := reg.SubjectOf(w)
			if err != nil {
				continue
			}
			cond := func([]Value) bool { return true }
			if w.When != nil {
				wc := &ctx{op: c.op, after: c.after && x.On == "", reads: c.reads}
				if cond, err = m.boolFn(w.When, ws, wc); err != nil {
					return nil, err
				}
			}
			inner := f
			f = func(s []Value) bool { return cond(s) || inner(s) }
		}
		return f, nil
	}
	written := func(a *ir.Attr) bool { return a != nil && a.Controller == ir.System }

	var defaults []func(c *ctx) pred
	var defaultIDs []string
	for _, e := range sortedEntities(reg) {
		for _, n := range e.AttrNames() {
			a := e.Attrs[n]
			var live []ir.DeclaredDefault
			for _, d := range a.Defaults {
				if r := reg.Rule(d.By); r != nil && r.Live() && !retired[d.By] {
					live = append(live, d)
				}
			}
			if len(live) > 1 {
				// Different defaults state different values of one default.
				dv := &Var{Name: a.QName() + ".default", Attr: a}
				for _, d := range live {
					v, err := literalFor(d.Value, a)
					if err != nil {
						return nil, err
					}
					dv.Domain = append(dv.Domain, v)
				}
				m.Vars = append(m.Vars, dv)
				idx := len(m.Vars) - 1
				m.index[dv.Name] = idx
				for _, d := range live {
					want, _ := literalFor(d.Value, a)
					defaultIDs = append(defaultIDs, d.By)
					defaults = append(defaults, func(c *ctx) pred {
						c.reads[idx] = true
						return func(s []Value) bool { return s[idx] == want }
					})
				}
				continue
			}
			if !assumeDefaults || len(live) == 0 || written(a) {
				continue
			}
			want, err := literalFor(live[0].Value, a)
			if err != nil {
				return nil, err
			}
			attr := a
			defaultIDs = append(defaultIDs, live[0].By)
			defaults = append(defaults, func(c *ctx) pred {
				i := m.varFor(attr, false)
				c.reads[i] = true
				return func(s []Value) bool { return s[i] == want }
			})
		}
	}

	// State unit, and the assumptions unit: every assumption as a rule, what
	// they assert as the response and what they only read as the situation.
	su := &Unit{Name: "state"}
	au := &Unit{Name: "assumptions"}
	aReads := map[int]bool{}
	aAsserted := map[int]bool{}
	stateVars := map[int]bool{}
	for _, x := range live {
		if x.Kind != ir.Invariant || x.On != "" {
			continue
		}
		c := &ctx{reads: map[int]bool{}, asserted: map[int]bool{}}
		f, err := compileRule(x, c)
		if err != nil {
			return nil, err
		}
		system := false
		for i := range c.reads {
			stateVars[i] = true
			system = system || (m.Vars[i].Attr != nil && written(m.Vars[i].Attr))
		}
		if system {
			su.Rules = append(su.Rules, &Rule{ID: x.ID, Reads: m.names(c.reads), eval: f})
		} else {
			su.assumed = append(su.assumed, f)
			au.Rules = append(au.Rules, &Rule{ID: x.ID, Reads: m.names(c.reads), eval: f})
			for i := range c.reads {
				aReads[i] = true
			}
			for i := range c.asserted {
				aAsserted[i] = true
			}
		}
	}
	for i, d := range defaults {
		c := &ctx{reads: map[int]bool{}}
		p := d(c)
		su.assumed = append(su.assumed, p)
		au.Rules = append(au.Rules, &Rule{ID: defaultIDs[i], Reads: m.names(c.reads), eval: p})
		for i := range c.reads {
			stateVars[i] = true
			aReads[i] = true
			aAsserted[i] = true
		}
	}
	au.Response = keys(aAsserted)
	for i := range aReads {
		if !aAsserted[i] {
			au.Situation = append(au.Situation, i)
		}
	}
	sort.Ints(au.Situation)
	for i := range stateVars {
		if written(m.Vars[i].Attr) {
			su.Response = append(su.Response, i)
		} else {
			su.Situation = append(su.Situation, i)
		}
	}
	// Assumptions that conflict with each other are that unit's finding;
	// like the checker, every other unit is checked without them, or no
	// situation would exist and every other conflict would go unseen.
	conflicting := map[string]bool{}
	if len(au.Rules) > 0 {
		m.Units = append(m.Units, au)
		sets, err := m.MinimalConflictSets(au.Name)
		if err != nil {
			return nil, err
		}
		for _, set := range sets {
			for _, id := range set {
				conflicting[id] = true
			}
		}
	}
	if len(conflicting) > 0 {
		su.assumed = nil
		for _, r := range au.Rules {
			if !conflicting[r.ID] {
				su.assumed = append(su.assumed, r.eval)
			}
		}
	}
	m.Units = append(m.Units, su)

	// Step units.
	ops := make([]string, 0, len(reg.Operations))
	for n := range reg.Operations {
		ops = append(ops, n)
	}
	sort.Strings(ops)
	for _, name := range ops {
		op := reg.Operations[name]
		u := &Unit{Name: name}
		runs := m.runsVar(name)
		sit, resp := map[int]bool{}, map[int]bool{runs: true}
		add := func(reads map[int]bool) {
			for i := range reads {
				if m.Vars[i].Post || i == runs {
					resp[i] = true
				} else {
					sit[i] = true
				}
			}
		}
		var allows, denies, pres []pred
		for _, x := range live {
			if !x.AppliesTo(name) {
				continue
			}
			subject, err := reg.SubjectOf(x)
			if err != nil {
				return nil, err
			}
			c := &ctx{op: op, reads: map[int]bool{}}
			f, err := compileRule(x, c)
			if err != nil {
				return nil, err
			}
			if x.Kind != ir.Invariant {
				g := func(s []Value) bool { return true }
				if x.When != nil {
					gc := &ctx{op: op, reads: map[int]bool{}}
					if g, err = m.boolFn(x.When, subject, gc); err != nil {
						return nil, err
					}
					add(gc.reads)
				}
				switch {
				case x.Kind == ir.Precondition:
					pres = append(pres, g)
				case x.Effect == "deny":
					denies = append(denies, g)
				default: // allow, and possible: permitted where it must be possible
					allows = append(allows, g)
				}
			}
			add(c.reads)
			c.reads[runs] = true
			if x.Effect == "possible" {
				u.possible = append(u.possible, &Rule{ID: x.ID, Reads: m.names(c.reads), eval: f})
				continue
			}
			u.Rules = append(u.Rules, &Rule{ID: x.ID, Reads: m.names(c.reads), eval: f})
		}
		if op.From != nil {
			i := m.varFor(op.From.Attr, false)
			from := op.From.Values
			sit[i] = true
			g := func(s []Value) bool { return slices.Contains(from, s[i].E) }
			pres = append(pres, g)
			u.Rules = append(u.Rules, &Rule{ID: name + ".from", Reads: m.names(map[int]bool{i: true, runs: true}),
				eval: func(s []Value) bool { return !s[runs].B || g(s) }})
		}
		if len(op.Effect) > 0 {
			reads := map[int]bool{runs: true}
			var sets []func([]Value) bool
			for _, as := range op.Effect {
				a := as.Attr
				if _, none := as.Value.(*ir.NoneLit); none && a.Optional {
					j := m.varAt(a.Entity.Name, a, "?", true)
					reads[j] = true
					sets = append(sets, func(s []Value) bool { return !s[j].B })
					continue
				}
				i := m.varFor(a, true)
				want, err := literalFor(as.Value, a)
				if err != nil {
					return nil, err
				}
				reads[i] = true
				sets = append(sets, func(s []Value) bool { return s[i] == want })
				if a.Optional {
					j := m.varAt(a.Entity.Name, a, "?", true)
					reads[j] = true
					sets = append(sets, func(s []Value) bool { return s[j].B })
				}
			}
			add(reads)
			u.Rules = append(u.Rules, &Rule{ID: name + ".effect", Reads: m.names(reads), eval: func(s []Value) bool {
				if !s[runs].B {
					return true
				}
				for _, f := range sets {
					if !f(s) {
						return false
					}
				}
				return true
			}})
		}
		if h := op.History; h != nil {
			ran, count, last := m.varFor(h.Ran, true), m.varFor(h.Runs, false), m.varFor(h.Runs, true)
			at, set, now := m.varFor(h.Last, true), m.varAt(h.Last.Entity.Name, h.Last, "?", true), m.varFor(ir.Now, false)
			reads := map[int]bool{runs: true, ran: true, count: true, last: true, at: true, set: true, now: true}
			add(reads)
			u.Rules = append(u.Rules, &Rule{ID: name + ".history", Reads: m.names(reads), eval: func(s []Value) bool {
				next := s[count].I + 1
				if s[count].I >= ir.MaxRuns {
					next = s[count].I
				}
				return !s[runs].B || (s[ran].B && s[last].I == next && s[at].I == s[now].I && s[set].B)
			}})
		}
		// Every state invariant holds before (assumed) and after (checked),
		// except a group of them that is unsatisfiable on its own and shares
		// no variable with the step: that contradiction is the state unit's
		// finding, and assuming it would make every step vacuous. Where the
		// invariants are satisfiable this assumes all of them, unsliced.
		var invs []stateInv
		for _, x := range live {
			if x.Kind != ir.Invariant || x.On != "" || conflicting[x.ID] {
				continue
			}
			pc := &ctx{op: op, reads: map[int]bool{}}
			pf, err := compileRule(x, pc)
			if err != nil {
				return nil, err
			}
			invs = append(invs, stateInv{x, pf, pc.reads})
			// As the checker assumes it: over the other instances too
			// (ISSUES 81), before the step only, since a step never writes
			// another instance.
			if lifted := m.reg.ForOtherInstances(x); lifted != nil {
				lc := &ctx{op: op, reads: map[int]bool{}}
				lf, err := compileRule(lifted, lc)
				if err != nil {
					return nil, err
				}
				u.assumed = append(u.assumed, lf)
				u.assumedID = append(u.assumedID, lifted.ID)
				add(lc.reads)
			}
		}
		drop := m.unrelatedContradictions(op, invs, sit, resp)
		for k, v := range invs {
			if drop[k] {
				continue
			}
			x, pf := v.x, v.f
			add(v.reads)
			u.assumed = append(u.assumed, pf)
			u.assumedID = append(u.assumedID, x.ID)
			system := false
			for i := range v.reads {
				system = system || written(m.Vars[i].Attr)
			}
			if !system {
				continue // an environment assumption; it cannot change in a step
			}
			qc := &ctx{op: op, after: true, reads: map[int]bool{}}
			qf, err := compileRule(x, qc)
			if err != nil {
				return nil, err
			}
			add(qc.reads)
			qc.reads[runs] = true
			u.Rules = append(u.Rules, &Rule{ID: x.ID, Reads: m.names(qc.reads), eval: func(s []Value) bool { return !s[runs].B || qf(s) }})
		}
		for i, d := range defaults {
			if conflicting[defaultIDs[i]] {
				continue
			}
			c := &ctx{op: op, reads: map[int]bool{}}
			u.assumed = append(u.assumed, d(c))
			u.assumedID = append(u.assumedID, defaultIDs[i])
			add(c.reads)
		}
		// Frame: an operation that does not run changes nothing.
		var pairs [][2]int
		for i := range resp {
			if v := m.Vars[i]; v.Post && v.Prefix != "Result" { // a step's result has no frame
				pairs = append(pairs, [2]int{m.varAt(v.Prefix, v.Attr, v.Part, false), i})
			}
		}
		for _, p := range pairs {
			sit[p[0]] = true
		}
		u.fixed = append(u.fixed, func(s []Value) bool {
			if s[runs].B {
				return true
			}
			for _, p := range pairs {
				if s[p[0]] != s[p[1]] {
					return false
				}
			}
			return true
		})
		any := func(ps []pred, s []Value) bool {
			for _, p := range ps {
				if p(s) {
					return true
				}
			}
			return false
		}
		allPre := func(s []Value) bool {
			for _, p := range pres {
				if !p(s) {
					return false
				}
			}
			return true
		}
		if op.Default == "deny" {
			u.fixed = append(u.fixed, func(s []Value) bool { return any(allows, s) || !s[runs].B })
		} else {
			u.fixed = append(u.fixed, func(s []Value) bool { return any(denies, s) || !allPre(s) || s[runs].B })
		}
		u.Situation, u.Response = keys(sit), keys(resp)
		u.cands = []Response{{Var: runs, Value: Value{B: true}, Label: name + " runs"}}
		m.Units = append(m.Units, u)
	}
	for _, i := range su.Response {
		if v := m.Vars[i]; v.Attr != nil && v.Part == "" && v.Attr.Kind == ir.Enum {
			for _, val := range v.Domain {
				su.cands = append(su.cands, Response{Var: i, Value: val, Label: v.Name + " = " + val.E})
			}
		}
	}
	for _, u := range m.Units {
		if size(m, u.Situation, false) > MaxStates || size(m, u.Response, true) > MaxStates {
			return nil, fmt.Errorf("oracle: unit %s is too large to enumerate", u.Name)
		}
	}
	return m, nil
}

// stateInv is a state invariant compiled for a step unit, with what it reads.
type stateInv struct {
	x     *ir.Rule
	f     pred
	reads map[int]bool
}

// unrelatedContradictions marks the state invariants of a step unit that
// form a group unsatisfiable on its own and unconnected to the step: to
// what its rules read (sit, resp), and to the invariants that read what
// the operation writes, through shared variables, transitively (a variable
// after the step counts as the same variable before it). The lowering
// leaves such a group out of the step's question (docs/05, slicing).
func (m *Model) unrelatedContradictions(op *ir.Operation, invs []stateInv, sit, resp map[int]bool) map[int]bool {
	base := func(i int) int {
		if v := m.Vars[i]; v.Post {
			if j, ok := m.index[strings.TrimSuffix(v.Name, "'")]; ok {
				return j
			}
		}
		return i
	}
	reach := map[int]bool{}
	for _, set := range []map[int]bool{sit, resp} {
		for i := range set {
			reach[base(i)] = true
		}
	}
	taken := make([]bool, len(invs))
	for k, v := range invs {
		for i := range v.reads {
			if a := m.Vars[i].Attr; a != nil && op.WritesAttr(a) {
				taken[k] = true
			}
		}
		if taken[k] {
			for i := range v.reads {
				reach[base(i)] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for k, v := range invs {
			if taken[k] {
				continue
			}
			for i := range v.reads {
				if reach[base(i)] {
					taken[k], changed = true, true
					for j := range v.reads {
						reach[base(j)] = true
					}
					break
				}
			}
		}
	}
	// The rest, in groups that share variables; a group no assignment
	// satisfies is left out.
	group := make([]int, len(invs))
	for k := range group {
		group[k] = k
	}
	var find func(int) int
	find = func(k int) int {
		if group[k] != k {
			group[k] = find(group[k])
		}
		return group[k]
	}
	owner := map[int]int{}
	for k, v := range invs {
		if taken[k] {
			continue
		}
		for i := range v.reads {
			if o, ok := owner[base(i)]; ok {
				group[find(k)] = find(o)
			} else {
				owner[base(i)] = k
			}
		}
	}
	members := map[int][]int{}
	for k := range invs {
		if !taken[k] {
			members[find(k)] = append(members[find(k)], k)
		}
	}
	drop := map[int]bool{}
	for _, ks := range members {
		vars := map[int]bool{}
		var fs []pred
		for _, k := range ks {
			fs = append(fs, invs[k].f)
			for i := range invs[k].reads {
				vars[i] = true
			}
		}
		idx := keys(vars)
		if size(m, idx, false) > MaxStates {
			continue // too large to decide: assumed, as before
		}
		s := make([]Value, len(m.Vars))
		sat := false
		each(m, idx, s, func() bool {
			sat = holds(fs, s)
			return !sat
		})
		if !sat {
			for _, k := range ks {
				drop[k] = true
			}
		}
	}
	return drop
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func (m *Model) names(reads map[int]bool) []string {
	var out []string
	for i := range reads {
		out = append(out, m.Vars[i].Name)
	}
	sort.Strings(out)
	return out
}

func size(m *Model, idx []int, wide bool) int {
	n := 1
	for _, i := range idx {
		k := len(m.Vars[i].Domain)
		if wide && m.Vars[i].Wide != nil {
			k = len(m.Vars[i].Wide)
		}
		n *= k
		if n > MaxStates {
			return n
		}
	}
	return n
}

func sortedEntities(reg *ir.Registry) []*ir.Entity {
	out := make([]*ir.Entity, 0, len(reg.Entities))
	for _, e := range reg.Entities {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// each calls f with every assignment of idx in s; f returns false to stop.
func each(m *Model, idx []int, s []Value, f func() bool) bool { return eachIn(m, idx, s, false, f) }

// eachResponse is each over response variables, with their wide domains.
func eachResponse(m *Model, idx []int, s []Value, f func() bool) bool {
	return eachIn(m, idx, s, true, f)
}

func eachIn(m *Model, idx []int, s []Value, wide bool, f func() bool) bool {
	var rec func(k int) bool
	rec = func(k int) bool {
		if k == len(idx) {
			return f()
		}
		dom := m.Vars[idx[k]].Domain
		if w := m.Vars[idx[k]].Wide; wide && w != nil {
			dom = w
		}
		for _, v := range dom {
			s[idx[k]] = v
			if !rec(k + 1) {
				return false
			}
		}
		return true
	}
	return rec(0)
}

func holds(ps []pred, s []Value) bool {
	for _, p := range ps {
		if !p(s) {
			return false
		}
	}
	return true
}

// neededAssumptions is what holds before the step that the possible rule
// p's conflict with the rules in set needs, as the checker finds it: the
// assumptions in rule order, each dropped if the conflict stays without it.
func (m *Model) neededAssumptions(u *Unit, p *Rule, set uint, have []string) []string {
	order := make([]int, len(u.assumed))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool { return u.assumedID[order[x]] < u.assumedID[order[y]] })
	drop := make([]bool, len(u.assumed))
	feasible := func() bool {
		s := make([]Value, len(m.Vars))
		found := false
		each(m, u.Situation, s, func() bool {
			for i, a := range u.assumed {
				if !drop[i] && !a(s) {
					return true
				}
			}
			eachResponse(m, u.Response, s, func() bool {
				if !holds(u.fixed, s) || !p.eval(s) {
					return true
				}
				for i, r := range u.Rules {
					if set&(1<<i) != 0 && !r.eval(s) {
						return true
					}
				}
				found = true
				return false
			})
			return !found
		})
		return found
	}
	var out []string
	for _, i := range order {
		drop[i] = true
		if feasible() {
			drop[i] = false
			if id := u.assumedID[i]; id != "" && !slices.Contains(have, id) && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// Unit returns the unit with the given name, or nil.
func (m *Model) Unit(name string) *Unit {
	for _, u := range m.Units {
		if u.Name == name {
			return u
		}
	}
	return nil
}

// masks enumerates, for each allowed situation, the sets of rules some
// response satisfies (as bitmasks over u.Rules), calling f once per situation.
func (m *Model) masks(u *Unit, f func(sit []Value, ms []uint) bool) {
	s := make([]Value, len(m.Vars))
	each(m, u.Situation, s, func() bool {
		if !holds(u.assumed, s) {
			return true
		}
		seen := map[uint]bool{}
		var ms []uint
		eachResponse(m, u.Response, s, func() bool {
			if !holds(u.fixed, s) {
				return true
			}
			var mask uint
			for i, r := range u.Rules {
				if r.eval(s) {
					mask |= 1 << i
				}
			}
			if !seen[mask] {
				seen[mask] = true
				ms = append(ms, mask)
			}
			return true
		})
		return f(s, ms)
	})
}

// MinimalConflictSets returns every minimal set of rules in the unit for
// which some allowed situation has no response satisfying them, each sorted.
func (m *Model) MinimalConflictSets(unit string) ([][]string, error) {
	u := m.Unit(unit)
	if u == nil {
		return nil, fmt.Errorf("oracle: no unit %s", unit)
	}
	n := len(u.Rules)
	if n > 14 {
		return nil, fmt.Errorf("oracle: %d rules in %s is too many", n, unit)
	}
	conflict := make([]bool, 1<<n)
	m.masks(u, func(_ []Value, ms []uint) bool {
		for set := uint(0); set < 1<<n; set++ {
			if conflict[set] {
				continue
			}
			ok := false
			for _, mk := range ms {
				if mk&set == set {
					ok = true
					break
				}
			}
			conflict[set] = !ok
		}
		return true
	})
	var out [][]string
	// A possible permission conflicts with the minimal sets of rules no
	// situation and response can satisfy together with it.
	for _, p := range u.possible {
		feasible := make([]bool, 1<<n)
		s := make([]Value, len(m.Vars))
		each(m, u.Situation, s, func() bool {
			if !holds(u.assumed, s) {
				return true
			}
			eachResponse(m, u.Response, s, func() bool {
				if !holds(u.fixed, s) || !p.eval(s) {
					return true
				}
				var mask uint
				for i, r := range u.Rules {
					if r.eval(s) {
						mask |= 1 << i
					}
				}
				for set := uint(0); set < 1<<n; set++ {
					if mask&set == set {
						feasible[set] = true
					}
				}
				return true
			})
			return true
		})
		for set := uint(0); set < 1<<n; set++ {
			if feasible[set] {
				continue
			}
			minimal := true
			for i := 0; i < n && minimal; i++ {
				if set&(1<<i) != 0 && !feasible[set&^(1<<i)] {
					minimal = false
				}
			}
			if minimal {
				ids := append(idsOf(u, set), p.ID)
				ids = append(ids, m.neededAssumptions(u, p, set, ids)...)
				sort.Strings(ids)
				out = append(out, ids)
			}
		}
	}
	for set := uint(0); set < 1<<n; set++ {
		if !conflict[set] {
			continue
		}
		minimal := true
		for i := 0; i < n && minimal; i++ {
			if set&(1<<i) != 0 && conflict[set&^(1<<i)] {
				minimal = false
			}
		}
		if minimal {
			out = append(out, idsOf(u, set))
		}
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
	return out, nil
}

func idsOf(u *Unit, set uint) []string {
	seen := map[string]bool{}
	var ids []string
	for i, r := range u.Rules {
		if set&(1<<i) != 0 && !seen[r.ID] {
			seen[r.ID] = true
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (m *Model) pick(u *Unit, ids []string) (uint, error) {
	var set uint
	for _, id := range ids {
		found := false
		for i, r := range u.Rules {
			if r.ID == id {
				set |= 1 << i
				found = true
			}
		}
		if !found {
			return 0, fmt.Errorf("oracle: %s is not a rule of %s", id, u.Name)
		}
	}
	return set, nil
}

// ConflictsAt reports whether the rules conflict in some allowed situation
// that agrees with the given values (variable name to value, as the checker
// prints them). Situation variables not given range over their domains, so a
// counterexample that names only what its rules read can be checked.
func (m *Model) ConflictsAt(unit string, ids []string, given map[string]string) (bool, error) {
	u := m.Unit(unit)
	if u == nil {
		return false, fmt.Errorf("oracle: no unit %s", unit)
	}
	set, err := m.pick(u, ids)
	if err != nil {
		return false, err
	}
	s := make([]Value, len(m.Vars))
	var free []int
	for _, i := range u.Situation {
		v := m.Vars[i]
		str, ok := given[v.Name]
		if !ok {
			free = append(free, i)
			continue
		}
		found := false
		for _, d := range v.Domain {
			if v.Format(d) == str {
				s[i], found = d, true
			}
		}
		if !found {
			return false, fmt.Errorf("oracle: %s=%s is outside its domain", v.Name, str)
		}
	}
	conflict := false
	each(m, free, s, func() bool {
		if !holds(u.assumed, s) {
			return true
		}
		sat := false
		eachResponse(m, u.Response, s, func() bool {
			if holds(u.fixed, s) {
				ok := true
				for i, r := range u.Rules {
					if set&(1<<i) != 0 && !r.eval(s) {
						ok = false
						break
					}
				}
				if ok {
					sat = true
					return false
				}
			}
			return true
		})
		conflict = !sat
		return !conflict
	})
	return conflict, nil
}

// Dead returns the labels of every dead response in the unit.
func (m *Model) Dead(unit string) []string {
	u := m.Unit(unit)
	var out []string
	for _, c := range u.cands {
		if ok, _ := m.Reachable(unit, c.Label, nil); !ok {
			out = append(out, c.Label)
		}
	}
	sort.Strings(out)
	return out
}

// Reachable reports whether the response named label happens in some
// allowed situation under the given rules (all rules when ids is nil).
func (m *Model) Reachable(unit, label string, ids []string) (bool, error) {
	u := m.Unit(unit)
	if u == nil {
		return false, fmt.Errorf("oracle: no unit %s", unit)
	}
	var cand *Response
	for i := range u.cands {
		if u.cands[i].Label == label {
			cand = &u.cands[i]
		}
	}
	if cand == nil {
		return false, fmt.Errorf("oracle: %s is not a candidate in %s", label, unit)
	}
	set := uint(1)<<len(u.Rules) - 1
	if ids != nil {
		var err error
		if set, err = m.pick(u, ids); err != nil {
			return false, err
		}
	}
	found := false
	m.masks(u, func(s []Value, _ []uint) bool {
		eachResponse(m, u.Response, s, func() bool {
			if s[cand.Var] != cand.Value || !holds(u.fixed, s) {
				return true
			}
			for i, r := range u.Rules {
				if set&(1<<i) != 0 && !r.eval(s) {
					return true
				}
			}
			found = true
			return false
		})
		return !found
	})
	return found, nil
}

// Reads returns the variables rule id reads in the unit, or nil.
func (m *Model) Reads(unit, id string) []string {
	u := m.Unit(unit)
	if u == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, r := range u.Rules {
		if r.ID == id {
			for _, n := range r.Reads {
				seen[n] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return keysS(seen)
}

func keysS(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Domain renders a variable's domain, or "" if the variable is unknown.
func (m *Model) Domain(name string) string {
	i, ok := m.index[name]
	if !ok {
		return ""
	}
	parts := make([]string, len(m.Vars[i].Domain))
	for k, v := range m.Vars[i].Domain {
		parts[k] = m.Vars[i].Format(v)
	}
	return strings.Join(parts, ",")
}

// --- interpreter -------------------------------------------------------------

func (m *Model) ruleFn(x *ir.Rule, subject *ir.Entity, c *ctx) (pred, error) {
	guard := func([]Value) bool { return true }
	if x.When != nil {
		pre := &ctx{op: c.op, after: c.after && x.On == "", reads: c.reads}
		g, err := m.boolFn(x.When, subject, pre)
		if err != nil {
			return nil, err
		}
		guard = g
	}
	switch x.Kind {
	case ir.Invariant:
		post := &ctx{op: c.op, after: c.after || x.On != "", reads: map[int]bool{}}
		a, err := m.boolFn(x.Assert, subject, post)
		if err != nil {
			return nil, err
		}
		for i := range post.reads {
			c.reads[i] = true
			if c.asserted != nil {
				c.asserted[i] = true
			}
		}
		if x.On != "" {
			runs := m.runsVar(opName(x.On, c))
			c.reads[runs] = true
			return func(s []Value) bool { return !s[runs].B || !guard(s) || a(s) }, nil
		}
		return func(s []Value) bool { return !guard(s) || a(s) }, nil
	case ir.Permission, ir.Precondition:
		runs := m.runsVar(opName(x.Operation, c))
		c.reads[runs] = true
		switch {
		case x.Kind == ir.Precondition:
			return func(s []Value) bool { return !s[runs].B || guard(s) }, nil
		case x.Effect == "deny":
			return func(s []Value) bool { return !guard(s) || !s[runs].B }, nil
		case x.Effect == "possible":
			// What must be possible: the guard holds and the operation runs.
			return func(s []Value) bool { return guard(s) && s[runs].B }, nil
		default:
			return func(s []Value) bool { return !guard(s) || s[runs].B }, nil
		}
	}
	return nil, fmt.Errorf("oracle: rule %s: kind %s", x.ID, x.Kind)
}

// opName is the operation a rule is about in this unit: its own, or for a
// pattern ("*", "Entity.*") the unit's.
func opName(name string, c *ctx) string {
	if ir.OpPattern(name) && c.op != nil {
		return c.op.Name
	}
	return name
}

func (m *Model) boolFn(e ir.Expr, subject *ir.Entity, c *ctx) (pred, error) {
	f, err := m.compile(e, subject, c)
	if err != nil {
		return nil, err
	}
	return guarded(f.set, func(s []Value) bool { return f.f(s).B }), nil
}

// cfn is a compiled expression and its static type.
type cfn struct {
	f      evalFn
	kind   ir.Kind
	t      *ir.Attr // declared type, when there is one
	lit    bool     // a bare enum literal
	numLit bool     // an integer literal
	none   bool
	items  []cfn  // a list literal's elements
	set    evalFn // an optional value's presence
	keys   evalFn // a map's present keys
	coll   *oslots
}

type oslots struct {
	rel  *ir.Relation
	from oplace
}

type oplace struct {
	entity *ir.Entity
	prefix string
	slot   bool
	via    *ir.Relation
	owner  *oplace
	cond   evalFn // the optional relations walked all lead somewhere; nil: always
}

func andFn(a, b evalFn) evalFn {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return func(s []Value) Value { return Value{B: a(s).B && b(s).B} }
}

// presenceFn holds when every optional operand has a value; nil: always.
func presenceFn(xs ...cfn) evalFn {
	var out evalFn
	for _, x := range xs {
		out = andFn(out, x.set)
	}
	return out
}

func guarded(set evalFn, p pred) pred {
	if set == nil {
		return p
	}
	return func(s []Value) bool { return set(s).B && p(s) }
}

type obind struct {
	name string
	val  *cfn
	at   *oplace
}

func constant(v Value, kind ir.Kind) cfn {
	return cfn{f: func([]Value) Value { return v }, kind: kind}
}

func (m *Model) value(c *ctx, at oplace, a *ir.Attr) cfn {
	// What a step produces is part of its response, read after the step
	// (D-064); the lowering rejects reading it anywhere else.
	output := a.Entity != nil && a.Entity.Name == "Result" && c.op != nil
	read := func(part string) evalFn {
		post := !at.slot && (output || c.after && c.op != nil && c.op.WritesAttr(a))
		i := m.varAt(at.prefix, a, part, post)
		c.reads[i] = true
		return func(s []Value) Value { return s[i] }
	}
	out := cfn{f: read(""), kind: a.Kind, t: a, set: at.cond}
	if a.Optional {
		out.set = andFn(at.cond, read("?"))
	}
	if a.Kind == ir.Map {
		out.keys = read("#keys")
	}
	return out
}

func (m *Model) path(p *ir.Path, subject *ir.Entity, c *ctx) (cfn, error) {
	var at oplace
	root := p.Segs[0]
	var bound *obind
	for i := len(c.scope) - 1; i >= 0; i-- {
		if c.scope[i].name == root {
			bound = &c.scope[i]
			break
		}
	}
	switch {
	case bound != nil && bound.val != nil:
		if len(p.Segs) > 1 {
			return cfn{}, fmt.Errorf("oracle: %s: a value has no attributes", p)
		}
		return *bound.val, nil
	case bound != nil:
		at = *bound.at
	default:
		var e *ir.Entity
		if root == "self" {
			e = subject
		} else {
			if _, err := m.reg.ResolveRef(p, subject); err != nil {
				return cfn{}, err
			}
			for name, x := range m.reg.Entities {
				if lowerFirst(name) == root || (root == "request" && name == "Request") || (root == "actor" && name == "Actor") {
					e = x
				}
			}
		}
		if e == nil {
			return cfn{}, fmt.Errorf("oracle: %s: no subject", p)
		}
		at = oplace{entity: e, prefix: e.Name}
	}
	segs := p.Segs[1:]
	for i, s := range segs {
		last := i == len(segs)-1
		if a, ok := at.entity.Attrs[s]; ok && last {
			return m.value(c, at, a), nil
		}
		rel, ok := at.entity.Relations[s]
		if !ok {
			return cfn{}, fmt.Errorf("oracle: %s: no %s", p, s)
		}
		to := m.reg.Entities[rel.To]
		var relSet evalFn
		if rel.Optional {
			relSet = m.value(c, oplace{entity: at.entity, prefix: at.prefix, slot: at.slot}, rel.Set).f
		}
		if last && rel.Optional {
			return cfn{set: andFn(at.cond, relSet), kind: 100}, nil
		}
		cond := andFn(at.cond, relSet)
		switch {
		case rel.Many && last:
			return cfn{coll: &oslots{rel: rel, from: at}}, nil
		case rel.Many || last:
			return cfn{}, fmt.Errorf("oracle: %s: bad path", p)
		case rel.Record && at.slot:
			at = oplace{entity: to, prefix: at.prefix + "." + s, slot: true, via: at.via, owner: at.owner, cond: cond}
		case at.slot && at.via != nil && at.owner != nil && rel.To == at.owner.entity.Name && rel.Inverse == at.via.Name:
			at = *at.owner
		default:
			at = oplace{entity: to, prefix: to.Name, cond: cond}
		}
	}
	return cfn{}, fmt.Errorf("oracle: %s: a path must end at an attribute", p)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func rat(v Value, kind ir.Kind) *big.Rat {
	if kind == ir.Decimal {
		r, _ := new(big.Rat).SetString(v.D)
		return r
	}
	return new(big.Rat).SetInt64(int64(v.I))
}

func decimal(r *big.Rat) Value { return Value{D: r.RatString()} }

// coerce turns x into a value of type t: an enum literal is already a name;
// an integer becomes a decimal where t is one.
func coerce(x cfn, t *ir.Attr) evalFn {
	if t != nil && t.Kind == ir.Decimal && x.kind == ir.Int {
		return func(s []Value) Value { return decimal(rat(x.f(s), ir.Int)) }
	}
	return x.f
}

func (m *Model) compile(e ir.Expr, subject *ir.Entity, c *ctx) (cfn, error) {
	switch e := e.(type) {
	case *ir.BoolLit:
		return constant(Value{B: e.V}, ir.Bool), nil
	case *ir.IntLit:
		x := constant(Value{I: e.V}, ir.Int)
		x.numLit = true
		return x, nil
	case *ir.DecLit:
		r, ok := new(big.Rat).SetString(e.Text)
		if !ok {
			return cfn{}, fmt.Errorf("oracle: decimal %s", e.Text)
		}
		return constant(decimal(r), ir.Decimal), nil
	case *ir.StrLit:
		return constant(Value{S: e.V}, ir.String), nil
	case *ir.DurLit:
		return constant(Value{I: e.MS}, ir.Duration), nil
	case *ir.TimeLit:
		return constant(Value{I: e.MS}, ir.Time), nil
	case *ir.NoneLit:
		return cfn{none: true}, nil
	case *ir.ListLit:
		x := cfn{kind: ir.List, items: []cfn{}}
		for _, el := range e.Elems {
			c, err := m.compile(el, subject, c)
			if err != nil {
				return cfn{}, err
			}
			x.items = append(x.items, c)
		}
		return x, nil
	case *ir.Ident:
		if e.Name == "now" {
			bound := false
			for _, b := range c.scope {
				bound = bound || b.name == "now"
			}
			if !bound {
				i := m.varFor(ir.Now, false)
				c.reads[i] = true
				return cfn{f: func(s []Value) Value { return s[i] }, kind: ir.Time, t: ir.Now}, nil
			}
		}
		for i := len(c.scope) - 1; i >= 0; i-- {
			if c.scope[i].name == e.Name {
				return m.path(&ir.Path{Segs: []string{e.Name}}, subject, c)
			}
		}
		x := constant(Value{E: e.Name}, ir.Enum)
		x.lit = true
		return x, nil
	case *ir.Path:
		return m.path(e, subject, c)
	case *ir.Not:
		x, err := m.compile(e.X, subject, c)
		if err != nil {
			return cfn{}, err
		}
		return cfn{f: func(s []Value) Value { return Value{B: !x.f(s).B} }, kind: ir.Bool}, nil
	case *ir.Scale:
		x, err := m.compile(e.X, subject, c)
		if err != nil {
			return cfn{}, err
		}
		k := e.K
		if x.kind == ir.Decimal {
			return cfn{f: func(s []Value) Value {
				return decimal(new(big.Rat).Mul(rat(x.f(s), ir.Decimal), new(big.Rat).SetInt64(int64(k))))
			}, kind: ir.Decimal, set: x.set}, nil
		}
		return cfn{f: func(s []Value) Value { return Value{I: x.f(s).I * k} }, kind: x.kind, set: x.set}, nil
	case *ir.Index:
		mp, err := m.compile(e.X, subject, c)
		if err != nil {
			return cfn{}, err
		}
		k, err := m.compile(e.K, subject, c)
		if err != nil {
			return cfn{}, err
		}
		if mp.kind == ir.List {
			elem := mp.t.Elem
			in := func(s []Value) Value {
				i := k.f(s).I
				return Value{B: i >= 0 && i < len(m.comp[mp.f(s).C])}
			}
			return cfn{f: func(s []Value) Value {
				if !in(s).B {
					return m.domain(elem)[0] // unspecified, as seq.nth past the end
				}
				return m.comp[mp.f(s).C][k.f(s).I]
			}, kind: elem.Kind, t: elem, set: andFn(presenceFn(mp, k), in)}, nil
		}
		key := coerce(k, mp.t.Key)
		return cfn{f: func(s []Value) Value {
			pairs := m.comp[mp.f(s).C]
			want := key(s)
			for i := 0; i+1 < len(pairs); i += 2 {
				if pairs[i] == want {
					return pairs[i+1]
				}
			}
			panic("oracle: key outside the map's domain")
		}, kind: mp.t.Elem.Kind, t: mp.t.Elem, set: presenceFn(mp, k)}, nil
	case *ir.Call:
		return m.call(e, subject, c)
	case *ir.Quant:
		return m.quant(e, subject, c)
	case *ir.Binary:
		return m.binary(e, subject, c)
	}
	return cfn{}, fmt.Errorf("oracle: unsupported expression %s", e)
}

func (m *Model) call(e *ir.Call, subject *ir.Entity, c *ctx) (cfn, error) {
	if e.Fn == "isSet" || e.Fn == "has" || e.Fn == "old" || ir.HistoryFunctions[e.Fn] {
		return m.call0(e, subject, c)
	}
	t, err := m.call0(e, subject, c)
	if err != nil {
		return t, err
	}
	var args []cfn
	for _, x := range e.Args {
		a, err := m.compile(x, subject, c)
		if err != nil {
			return t, err
		}
		args = append(args, a)
	}
	set := presenceFn(args...)
	if t.kind == ir.Bool && set != nil {
		f := t.f
		t.f = func(s []Value) Value { return Value{B: set(s).B && f(s).B} }
	} else if t.kind != ir.Bool {
		t.set = set
	}
	return t, nil
}

func (m *Model) call0(e *ir.Call, subject *ir.Entity, c *ctx) (cfn, error) {
	switch e.Fn {
	case "old":
		before := *c
		before.after = false
		return m.compile(e.Args[0], subject, &before)
	case "ran", "runs", "lastRun":
		p, _ := e.Args[0].(*ir.Path)
		var op *ir.Operation
		if p != nil {
			op = m.reg.Operations[strings.Join(p.Segs, ".")]
		}
		if op == nil || op.History == nil {
			return cfn{}, fmt.Errorf("oracle: %s of an unknown operation", e.Fn)
		}
		at := oplace{entity: op.Entity, prefix: op.Entity.Name}
		switch e.Fn {
		case "ran":
			return m.value(c, at, op.History.Ran), nil
		case "runs":
			return m.value(c, at, op.History.Runs), nil
		}
		return m.value(c, at, op.History.Last), nil
	}
	var args []cfn
	for _, x := range e.Args {
		a, err := m.compile(x, subject, c)
		if err != nil {
			return cfn{}, err
		}
		args = append(args, a)
	}
	b := func(f func(s []Value) bool) (cfn, error) {
		return cfn{f: func(s []Value) Value { return Value{B: f(s)} }, kind: ir.Bool}, nil
	}
	switch e.Fn {
	case "startsWith":
		return b(func(s []Value) bool { return strings.HasPrefix(args[0].f(s).S, args[1].f(s).S) })
	case "endsWith":
		return b(func(s []Value) bool { return strings.HasSuffix(args[0].f(s).S, args[1].f(s).S) })
	case "contains":
		x := args[0]
		if x.kind == ir.String {
			return b(func(s []Value) bool { return strings.Contains(x.f(s).S, args[1].f(s).S) })
		}
		elem := coerce(args[1], x.t.Elem)
		return b(func(s []Value) bool { return slices.Contains(m.comp[x.f(s).C], elem(s)) })
	case "len":
		x := args[0]
		if x.kind == ir.String {
			return cfn{f: func(s []Value) Value { return Value{I: utf8.RuneCountInString(x.f(s).S)} }, kind: ir.Int}, nil
		}
		return cfn{f: func(s []Value) Value { return Value{I: len(m.comp[x.f(s).C])} }, kind: ir.Int}, nil
	case "isEmpty":
		x := args[0]
		switch {
		case x.kind == ir.String:
			return b(func(s []Value) bool { return x.f(s).S == "" })
		case x.kind == ir.Map:
			return b(func(s []Value) bool { return len(m.comp[x.keys(s).C]) == 0 })
		}
		return b(func(s []Value) bool { return len(m.comp[x.f(s).C]) == 0 })
	case "matches":
		re, err := regexp.Compile("^(?:" + e.Args[1].(*ir.StrLit).V + ")$")
		if err != nil {
			return cfn{}, err
		}
		return b(func(s []Value) bool { return re.MatchString(args[0].f(s).S) })
	case "isSet":
		if args[0].set == nil {
			return cfn{}, fmt.Errorf("oracle: isSet of a value that is not optional")
		}
		set := args[0].set
		return b(func(s []Value) bool { return set(s).B })
	case "has":
		keys, key := args[0].keys, coerce(args[1], args[0].t.Key)
		return b(func(s []Value) bool { return slices.Contains(m.comp[keys(s).C], key(s)) })
	case "seconds", "minutes", "hours", "days":
		ms := map[string]int{"seconds": 1000, "minutes": 60_000, "hours": 3_600_000, "days": 86_400_000}[e.Fn]
		x := args[0]
		return cfn{f: func(s []Value) Value { return Value{I: ms * x.f(s).I} }, kind: ir.Duration}, nil
	case "min", "max", "abs":
		// Of one kind, as the lowering types it: a decimal makes the result
		// a decimal; an int literal takes the other argument's kind.
		k := args[0].kind
		for _, a := range args[1:] {
			if a.kind == ir.Decimal || k == ir.Decimal {
				k = ir.Decimal
			} else if k == ir.Int {
				k = a.kind
			}
		}
		pick := func(s []Value) (*big.Rat, *big.Rat, Value, Value) {
			x, y := args[0].f(s), args[len(args)-1].f(s)
			return rat(x, args[0].kind), rat(y, args[len(args)-1].kind), x, y
		}
		out := func(r *big.Rat, v Value) Value {
			if k == ir.Decimal {
				return decimal(r)
			}
			return v
		}
		switch e.Fn {
		case "min", "max":
			lt := e.Fn == "min"
			return cfn{f: func(s []Value) Value {
				x, y, xv, yv := pick(s)
				if (x.Cmp(y) <= 0) == lt {
					return out(x, xv)
				}
				return out(y, yv)
			}, kind: k}, nil
		}
		return cfn{f: func(s []Value) Value {
			x, _, xv, _ := pick(s)
			if x.Sign() >= 0 {
				return out(x, xv)
			}
			if k == ir.Decimal {
				return decimal(new(big.Rat).Neg(x))
			}
			return Value{I: -xv.I}
		}, kind: k}, nil
	}
	return cfn{}, fmt.Errorf("oracle: function %s", e.Fn)
}

func (m *Model) quant(e *ir.Quant, subject *ir.Entity, c *ctx) (cfn, error) {
	coll, err := m.compile(e.Coll, subject, c)
	if err != nil {
		return cfn{}, err
	}
	type item struct {
		in pred
		b  obind
	}
	var items []item
	switch {
	case coll.coll != nil:
		rel, from := coll.coll.rel, coll.coll.from
		to := m.reg.Entities[rel.To]
		owner := from
		for i := 0; i < rel.Max; i++ {
			prefix := fmt.Sprintf("%s.%s#%d", from.prefix, rel.Name, i+1)
			in := m.flag(prefix + ":in")
			c.reads[in] = true
			items = append(items, item{in: func(s []Value) bool { return s[in].B }, b: obind{name: e.Var, at: &oplace{entity: to, prefix: prefix, slot: true, via: rel, owner: &owner}}})
		}
	case coll.kind == ir.List:
		set := coll.set // an unset optional collection has no elements
		for i := 0; i < coll.t.MaxLen; i++ {
			k := i
			v := cfn{f: func(s []Value) Value {
				elems := m.comp[coll.f(s).C]
				if k < len(elems) {
					return elems[k]
				}
				return m.domain(coll.t.Elem)[0] // unspecified, as seq.nth past the end
			}, kind: coll.t.Elem.Kind, t: coll.t.Elem}
			items = append(items, item{in: guarded(set, func(s []Value) bool { return k < len(m.comp[coll.f(s).C]) }), b: obind{name: e.Var, val: &v}})
		}
	case coll.kind == ir.SetKind:
		for _, mv := range m.domain(coll.t.Elem) {
			val := mv
			v := constant(val, coll.t.Elem.Kind)
			v.t = coll.t.Elem
			items = append(items, item{in: guarded(coll.set, func(s []Value) bool { return slices.Contains(m.comp[coll.f(s).C], val) }), b: obind{name: e.Var, val: &v}})
		}
	default:
		return cfn{}, fmt.Errorf("oracle: %s ranges over a %s", e.Op, ir.KindNames[coll.kind])
	}
	var bodies []pred
	for _, it := range items {
		c.scope = append(c.scope, it.b)
		body, err := m.compile(e.Body, subject, c)
		c.scope = c.scope[:len(c.scope)-1]
		if err != nil {
			return cfn{}, err
		}
		bodies = append(bodies, func(s []Value) bool { return body.f(s).B })
	}
	op := e.Op
	f := func(s []Value) Value {
		n := 0
		for i, it := range items {
			if it.in(s) && bodies[i](s) {
				n++
			} else if op == "all" && it.in(s) {
				return Value{B: false}
			}
		}
		switch op {
		case "all":
			return Value{B: true}
		case "any":
			return Value{B: n > 0}
		}
		return Value{I: n}
	}
	if op == "count" {
		return cfn{f: f, kind: ir.Int}, nil
	}
	return cfn{f: f, kind: ir.Bool}, nil
}

func (m *Model) binary(e *ir.Binary, subject *ir.Entity, c *ctx) (cfn, error) {
	l, err := m.compile(e.L, subject, c)
	if err != nil {
		return cfn{}, err
	}
	b := func(f func(s []Value) bool) (cfn, error) {
		return cfn{f: func(s []Value) Value { return Value{B: f(s)} }, kind: ir.Bool}, nil
	}
	if e.Op == "in" {
		set, ok := e.R.(*ir.Set)
		if !ok {
			return cfn{}, fmt.Errorf("oracle: in needs a set")
		}
		var eqs []pred
		for _, x := range set.Elems {
			r, err := m.compile(x, subject, c)
			if err != nil {
				return cfn{}, err
			}
			eqs = append(eqs, m.equalP(l, r))
		}
		return b(func(s []Value) bool {
			for _, eq := range eqs {
				if eq(s) {
					return true
				}
			}
			return false
		})
	}
	r, err := m.compile(e.R, subject, c)
	if err != nil {
		return cfn{}, err
	}
	switch e.Op {
	case "and":
		return b(func(s []Value) bool { return l.f(s).B && r.f(s).B })
	case "or":
		return b(func(s []Value) bool { return l.f(s).B || r.f(s).B })
	case "implies":
		return b(func(s []Value) bool { return !l.f(s).B || r.f(s).B })
	case "=", "!=":
		eq := m.equalP(l, r)
		if keptValue(e.L, e.R) && (l.set != nil || r.set != nil) {
			// x = old(x): kept, unset before and after included (the
			// checker's reading; D-052 otherwise).
			inner, ls, rs := eq, l.set, r.set
			unset := func(f evalFn, s []Value) bool { return f != nil && !f(s).B }
			eq = func(s []Value) bool { return inner(s) || unset(ls, s) && unset(rs, s) }
		}
		if e.Op == "!=" {
			return b(func(s []Value) bool { return !eq(s) })
		}
		return b(eq)
	case "<", "<=", ">", ">=":
		cmp := compare(l, r)
		op := e.Op
		return b(guarded(presenceFn(l, r), func(s []Value) bool {
			n := cmp(s)
			switch op {
			case "<":
				return n < 0
			case "<=":
				return n <= 0
			case ">":
				return n > 0
			}
			return n >= 0
		}))
	case "+", "-":
		set := presenceFn(l, r)
		if l.kind == ir.String {
			return cfn{f: func(s []Value) Value { return Value{S: l.f(s).S + r.f(s).S} }, kind: ir.String, set: set}, nil
		}
		kind := l.kind
		switch {
		case l.kind == ir.Time && r.kind == ir.Time:
			kind = ir.Duration
		case l.kind == ir.Duration && r.kind == ir.Time:
			kind = ir.Time
		case l.kind == ir.Decimal || r.kind == ir.Decimal:
			kind = ir.Decimal
		}
		sub := e.Op == "-"
		if kind == ir.Decimal {
			return cfn{f: func(s []Value) Value {
				x, y := rat(l.f(s), l.kind), rat(r.f(s), r.kind)
				if sub {
					return decimal(new(big.Rat).Sub(x, y))
				}
				return decimal(new(big.Rat).Add(x, y))
			}, kind: kind, set: set}, nil
		}
		return cfn{f: func(s []Value) Value {
			if sub {
				return Value{I: l.f(s).I - r.f(s).I}
			}
			return Value{I: l.f(s).I + r.f(s).I}
		}, kind: kind, set: set}, nil
	}
	return cfn{}, fmt.Errorf("oracle: operator %s", e.Op)
}

// equalP is equal, false where an operand is an unset optional value.
func (m *Model) equalP(l, r cfn) pred {
	if r.items != nil && l.items == nil {
		l, r = r, l
	}
	if l.items != nil && r.t != nil {
		// A list literal takes the collection type of the other side.
		items, kind := l.items, r.kind
		return guarded(r.set, func(s []Value) bool {
			var elems []Value
			for _, it := range items {
				elems = append(elems, coerce(it, r.t.Elem)(s))
			}
			want := m.compose("L", elems)
			if kind == ir.SetKind {
				sort.Slice(elems, func(i, j int) bool { return enc(elems[i]) < enc(elems[j]) })
				elems = slices.CompactFunc(elems, func(a, b Value) bool { return a == b })
				want = m.compose("S", elems)
			}
			return r.f(s) == want
		})
	}
	if l.none || r.none {
		return equal(l, r)
	}
	return guarded(presenceFn(l, r), equal(l, r))
}

// equal compares two values: none against an optional's presence, ints with
// decimals as numbers, and everything else by value (enum values by name).
func equal(l, r cfn) pred {
	if l.none || r.none {
		if l.none {
			l, r = r, l
		}
		if l.none {
			return func([]Value) bool { return true }
		}
		set := l.set
		return func(s []Value) bool { return !set(s).B }
	}
	if l.kind == ir.Decimal || r.kind == ir.Decimal {
		return func(s []Value) bool { return rat(l.f(s), l.kind).Cmp(rat(r.f(s), r.kind)) == 0 }
	}
	return func(s []Value) bool { return l.f(s) == r.f(s) }
}

func compare(l, r cfn) func([]Value) int {
	if l.kind == ir.String {
		return func(s []Value) int { return strings.Compare(l.f(s).S, r.f(s).S) }
	}
	return func(s []Value) int { return rat(l.f(s), l.kind).Cmp(rat(r.f(s), r.kind)) }
}

// literal is a default's or an effect's value.
func literal(e ir.Expr) (Value, error) {
	switch e := e.(type) {
	case *ir.IntLit:
		return Value{I: e.V}, nil
	case *ir.BoolLit:
		return Value{B: e.V}, nil
	case *ir.Ident:
		return Value{E: e.Name}, nil
	case *ir.StrLit:
		return Value{S: e.V}, nil
	case *ir.DurLit:
		return Value{I: e.MS}, nil
	case *ir.TimeLit:
		return Value{I: e.MS}, nil
	case *ir.DecLit:
		r, ok := new(big.Rat).SetString(e.Text)
		if !ok {
			return Value{}, fmt.Errorf("oracle: decimal %s", e.Text)
		}
		return decimal(r), nil
	}
	return Value{}, fmt.Errorf("oracle: literal %s", e)
}

// literalFor is literal, as a value of attribute a (an int default of a
// decimal attribute is a decimal).
func literalFor(e ir.Expr, a *ir.Attr) (Value, error) {
	v, err := literal(e)
	if err == nil && a.Kind == ir.Decimal {
		if _, ok := e.(*ir.IntLit); ok {
			return decimal(new(big.Rat).SetInt64(int64(v.I))), nil
		}
	}
	return v, err
}

// keptValue reports whether one side of a comparison is old() of the other.
func keptValue(a, b ir.Expr) bool {
	isOld := func(c, x ir.Expr) bool {
		call, ok := c.(*ir.Call)
		return ok && call.Fn == "old" && len(call.Args) == 1 && call.Args[0].String() == x.String()
	}
	return isOld(a, b) || isOld(b, a)
}
