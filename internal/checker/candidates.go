package checker

import (
	"math/big"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/solver"
)

// Beyond "unchanged" and "equals t", a rule can pin a response variable
// without an equality: two bounds on a whole number ("lentAt + 14d +
// days(n) <= now < lentAt + 14d + days(n+1)"), or a string it must contain
// ("the message names the overdue tool"). Refinement collected such a
// response as a constant, which answers only the situations with that
// value, so it never converged and the step was left undecided (ISSUES 80).
// These are the terms such rules give: the extreme value each linear bound
// on an integer allows, and the strings a value must contain, alone and
// joined.

// rawTerm is a term a response variable may take, which may still name
// other response variables.
type rawTerm struct {
	v        string
	t        solver.SExpr
	contains bool // a string the value must contain
	// key is set when v is a map (an SMT array) and t its value at key:
	// the entries a rule gives a per-item result, "dueDateOf[t.id] =
	// t.lentAt + 14d".
	key *solver.SExpr
	// when is the condition over the situation under which the rule gives
	// the entry, nil for always.
	when *solver.SExpr
}

// valueSort is the value sort of an SMT array sort, "" for any other.
func valueSort(sort string) string {
	es, err := solver.Parse(sort)
	if err != nil || len(es) != 1 || len(es[0].List) != 3 || es[0].List[0].Atom != "Array" {
		return ""
	}
	return es[0].List[2].String()
}

// mapEntry reads e as (select M k), M a response map: M and k.
func mapEntry(e solver.SExpr, sys map[string]bool) (string, solver.SExpr, bool) {
	if len(e.List) != 3 || e.List[0].Atom != "select" || e.List[1].IsList() || e.List[1].Str || !sys[e.List[1].Atom] {
		return "", solver.SExpr{}, false
	}
	return e.List[1].Atom, e.List[2], true
}

// maxTermsPerVar bounds the terms collected for one variable: each is asked
// for in every response query.
const maxTermsPerVar = 12

// boundTerms gives, for the comparison e (<=, <, >=, > over integers), each
// Int response variable it bounds linearly and the extreme value that bound
// allows it, as a term over everything else e names.
func boundTerms(e solver.SExpr, sorts map[string]string, sys map[string]bool) []rawTerm {
	if len(e.List) != 3 {
		return nil
	}
	a, okA := linearOf(e.List[1], sorts)
	b, okB := linearOf(e.List[2], sorts)
	if !okA || !okB {
		return nil
	}
	// As d <= 0 over the integers.
	var d *linear
	switch e.List[0].Atom {
	case "<=":
		d = a.minus(b)
	case "<":
		d = a.minus(b).plus(1)
	case ">=":
		d = b.minus(a)
	case ">":
		d = b.minus(a).plus(1)
	default:
		return nil
	}
	var out []rawTerm
	for name, c := range d.coef {
		if c.Sign() == 0 {
			continue
		}
		m, key, entry := mapEntry(d.atoms[name], sys)
		if !entry && (!sys[name] || sorts[name] != "Int") {
			continue
		}
		rest := d.without(name)
		if rest.mentions(name) {
			continue // not linear in it
		}
		// c*v + R <= 0, with N = -R: v <= floor(N/c) when c > 0, and
		// v >= ceil(N/c) = -floor(N/|c|) when c < 0.
		n := rest.neg().smt()
		abs := new(big.Int).Abs(c)
		t := n
		if abs.Cmp(big.NewInt(1)) != 0 {
			t = "(div " + n + " " + abs.String() + ")"
		}
		if c.Sign() < 0 {
			t = "(- " + t + ")"
		}
		es, err := solver.Parse(t)
		switch {
		case err != nil || len(es) != 1:
		case entry:
			out = append(out, rawTerm{v: m, t: es[0], key: &key})
		default:
			out = append(out, rawTerm{v: name, t: es[0]})
		}
	}
	return out
}

// linear is the sum of coef[a]*a over atoms a, plus k.
type linear struct {
	coef  map[string]*big.Int
	atoms map[string]solver.SExpr
	k     *big.Int
}

func newLinear() *linear {
	return &linear{coef: map[string]*big.Int{}, atoms: map[string]solver.SExpr{}, k: new(big.Int)}
}

// intHeads are the operators whose application is an integer term linear
// forms keep whole, as an atom.
var intHeads = map[string]bool{"str.len": true, "str.indexof": true, "str.to_int": true, "div": true, "mod": true, "abs": true}

// linearOf reads e as a linear integer term. Variables must be Int; any
// other application it cannot see into must be one of intHeads, or an ite
// whose branches are linear.
func linearOf(e solver.SExpr, sorts map[string]string) (*linear, bool) {
	l := newLinear()
	if !e.IsList() {
		if e.Str {
			return nil, false
		}
		if n, ok := new(big.Int).SetString(e.Atom, 10); ok {
			l.k = n
			return l, true
		}
		if sorts[e.Atom] != "Int" {
			return nil, false
		}
		l.coef[e.Atom], l.atoms[e.Atom] = big.NewInt(1), e
		return l, true
	}
	if len(e.List) == 0 {
		return nil, false
	}
	args := e.List[1:]
	switch head := e.List[0].Atom; head {
	case "+", "-":
		if len(args) == 0 {
			return nil, false
		}
		for i, x := range args {
			y, ok := linearOf(x, sorts)
			if !ok {
				return nil, false
			}
			if head == "-" && (i > 0 || len(args) == 1) {
				y = y.neg()
			}
			l = l.add(y)
		}
		return l, true
	case "*":
		l.k = big.NewInt(1)
		var term *linear
		for _, x := range args {
			y, ok := linearOf(x, sorts)
			if !ok {
				return nil, false
			}
			if len(y.coef) == 0 {
				l.k.Mul(l.k, y.k)
				continue
			}
			if term != nil {
				return nil, false // a product of two variables
			}
			term = y
		}
		if term == nil {
			return l, true
		}
		return term.scale(l.k), true
	case "ite":
		if len(args) != 3 {
			return nil, false
		}
		for _, x := range args[1:] {
			if _, ok := linearOf(x, sorts); !ok {
				return nil, false
			}
		}
	case "select":
		// A map's value at a key, when the map holds integers.
		if len(args) != 2 || args[0].IsList() || valueSort(sorts[args[0].Atom]) != "Int" {
			return nil, false
		}
	default:
		if !intHeads[head] {
			return nil, false
		}
	}
	key := e.String()
	l.coef[key], l.atoms[key] = big.NewInt(1), e
	return l, true
}

func (l *linear) clone() *linear {
	out := newLinear()
	for a, c := range l.coef {
		out.coef[a], out.atoms[a] = new(big.Int).Set(c), l.atoms[a]
	}
	out.k.Set(l.k)
	return out
}

func (l *linear) add(o *linear) *linear {
	out := l.clone()
	for a, c := range o.coef {
		if out.coef[a] == nil {
			out.coef[a], out.atoms[a] = new(big.Int), o.atoms[a]
		}
		out.coef[a].Add(out.coef[a], c)
	}
	out.k.Add(out.k, o.k)
	return out
}

func (l *linear) scale(n *big.Int) *linear {
	out := l.clone()
	for _, c := range out.coef {
		c.Mul(c, n)
	}
	out.k.Mul(out.k, n)
	return out
}

func (l *linear) neg() *linear            { return l.scale(big.NewInt(-1)) }
func (l *linear) minus(o *linear) *linear { return l.add(o.neg()) }
func (l *linear) plus(n int64) *linear    { out := l.clone(); out.k.Add(out.k, big.NewInt(n)); return out }
func (l *linear) without(a string) *linear {
	out := l.clone()
	delete(out.coef, a)
	delete(out.atoms, a)
	return out
}

// mentions reports whether an atom of l names the symbol s.
func (l *linear) mentions(s string) bool {
	for a, c := range l.coef {
		if c.Sign() != 0 && names(l.atoms[a], s) {
			return true
		}
	}
	return false
}

// smt writes l as an SMT-LIB term, atoms in a stable order.
func (l *linear) smt() string {
	var keys []string
	for a, c := range l.coef {
		if c.Sign() != 0 {
			keys = append(keys, a)
		}
	}
	sort.Strings(keys)
	num := func(n *big.Int) string {
		if n.Sign() < 0 {
			return "(- " + new(big.Int).Neg(n).String() + ")"
		}
		return n.String()
	}
	var parts []string
	for _, a := range keys {
		switch c := l.coef[a]; {
		case c.Cmp(big.NewInt(1)) == 0:
			parts = append(parts, a)
		case c.Cmp(big.NewInt(-1)) == 0:
			parts = append(parts, "(- "+a+")")
		default:
			parts = append(parts, "(* "+num(c)+" "+a+")")
		}
	}
	if l.k.Sign() != 0 || len(parts) == 0 {
		parts = append(parts, num(l.k))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(+ " + strings.Join(parts, " ") + ")"
}

// names reports whether e mentions the symbol s.
func names(e solver.SExpr, s string) bool {
	if !e.IsList() {
		return !e.Str && e.Atom == s
	}
	for _, x := range e.List {
		if names(x, s) {
			return true
		}
	}
	return false
}

// substitute replaces the symbol s in e by r.
func substitute(e solver.SExpr, s string, r solver.SExpr) solver.SExpr {
	if !e.IsList() {
		if !e.Str && e.Atom == s {
			return r
		}
		return e
	}
	out := solver.SExpr{List: make([]solver.SExpr, len(e.List))}
	for i, x := range e.List {
		out.List[i] = substitute(x, s, r)
	}
	return out
}
