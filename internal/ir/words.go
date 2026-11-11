package ir

import (
	"fmt"
	"strings"
	"unicode"
)

// Words says what a rule means in plain English, for an owner who approves
// it without reading rule syntax: "Refuse “lend” on a tool when the number
// of other tools that are lent to the requester is at least 3". A trial
// owner (2026-11-11) was shown `count(t in instances(Tool): ...) >= 3` and
// could not judge it. The rule syntax stays the record; this is how it is
// shown.
func (r *Rule) Words() string {
	w := &sayer{self: r.selfEntity()}
	var s string
	cond := func(prefix string) string {
		if r.When == nil {
			return ""
		}
		return prefix + w.cond(r.When)
	}
	step := stepWords(r.Operation)
	switch r.Kind {
	case Permission:
		switch r.Effect {
		case "deny":
			s = "Refuse " + step + cond(" when ")
		case "allow":
			s = "Allow " + step + cond(" when ")
		case "possible":
			s = capital(step) + " must be able to run" + cond(" when ")
		default:
			s = r.Effect + " " + step + cond(" when ")
		}
	case Precondition:
		s = capital(step) + " runs only when " + w.cond(orTrue(r.When))
	case Invariant, Derivation:
		body := w.cond(orTrue(r.Assert))
		switch {
		case r.On != "" && r.Assume:
			s = "When " + stepWords(r.On) + " runs" + cond(" and ") + ", it is assumed that " + body
		case r.On != "":
			s = "After " + stepWords(r.On) + cond(", when before it ") + ", " + body
		case r.When != nil:
			s = "Whenever " + w.cond(r.When) + ", " + body
		case r.Kind == Derivation:
			s = "By definition, " + body
		default:
			s = "Always, " + body
		}
	case Informal, Obligation:
		s = "Not checked against other requirements"
		if r.Rationale != "" {
			s += ": " + r.Rationale
		}
	default:
		s = r.Kind
	}
	if len(r.Overrides) > 0 {
		s += " (takes precedence over " + strings.Join(r.Overrides, ", ") + ")"
	}
	return s
}

func orTrue(e Expr) Expr {
	if e == nil {
		return &BoolLit{V: true}
	}
	return e
}

// selfEntity is the entity `self` names: the subject, else the entity of
// the rule's step.
func (r *Rule) selfEntity() string {
	if r.Subject != "" {
		return r.Subject
	}
	for _, op := range []string{r.Operation, r.On} {
		if ent, _, ok := strings.Cut(op, "."); ok {
			return ent
		}
	}
	return ""
}

// stepWords names a step: "Tool.lend" is “lend” on a tool.
func stepWords(op string) string {
	ent, name, ok := strings.Cut(op, ".")
	if !ok {
		return "“" + splitName(op) + "”"
	}
	return "“" + splitName(name) + "” on " + article(splitName(ent))
}

// StepWords is stepWords for other packages' messages.
func StepWords(op string) string { return stepWords(op) }

type sayer struct {
	self string
	// vars names what each quantified variable ranges over: "tool" for
	// t in instances(Tool), "" when unknown.
	vars map[string]string
}

func (w *sayer) cond(e Expr) string {
	switch e := e.(type) {
	case *Binary:
		switch e.Op {
		case "and":
			return w.cond(e.L) + " and " + w.cond(e.R)
		case "or":
			return "either " + w.cond(e.L) + " or " + w.cond(e.R)
		case "implies":
			return "if " + w.cond(e.L) + " then " + w.cond(e.R)
		}
		return w.compare(e)
	case *Not:
		switch x := e.X.(type) {
		case *Call:
			if x.Fn == "isSet" && len(x.Args) == 1 {
				return w.value(x.Args[0]) + " is not set"
			}
		case *Path:
			if s, ok := w.flag(x, true); ok {
				return s
			}
			return w.value(x) + " is false"
		case *Binary:
			if x.Op == "in" {
				return w.value(x.L) + " is none of " + w.value(x.R)
			}
		}
		return "not (" + w.cond(e.X) + ")"
	case *Path:
		if s, ok := w.flag(e, false); ok {
			return s
		}
		return w.value(e) + " is true"
	case *BoolLit:
		if e.V {
			return "always"
		}
		return "never"
	case *Call:
		if s, ok := w.call(e); ok {
			return s
		}
		return w.value(e) + " is true"
	case *Quant:
		coll, elem := w.collection(e)
		inner := w.with(e.Var, elem)
		switch e.Op {
		case "any":
			return "at least one of " + coll + " is such that " + inner.cond(e.Body)
		case "all":
			return "every one of " + coll + " is such that " + inner.cond(e.Body)
		}
	}
	return w.value(e)
}

// call says a built-in function that is a condition; ok is false for one
// that is a value.
func (w *sayer) call(e *Call) (string, bool) {
	switch {
	case e.Fn == "isSet" && len(e.Args) == 1:
		return w.value(e.Args[0]) + " is set", true
	case len(e.Args) == 2 && (e.Fn == "contains" || e.Fn == "has"):
		return w.value(e.Args[0]) + " contains " + w.value(e.Args[1]), true
	case len(e.Args) == 2 && e.Fn == "startsWith":
		return w.value(e.Args[0]) + " starts with " + w.value(e.Args[1]), true
	case len(e.Args) == 2 && e.Fn == "endsWith":
		return w.value(e.Args[0]) + " ends with " + w.value(e.Args[1]), true
	}
	return "", false
}

// flag says a yes/no fact named as one, "isSteward" or "hasHolds": the
// actor is steward, the tool has holds.
func (w *sayer) flag(p *Path, negated bool) (string, bool) {
	if len(p.Segs) < 2 {
		return "", false
	}
	verb, rest, ok := strings.Cut(splitName(p.Segs[len(p.Segs)-1]), " ")
	if !ok || (verb != "is" && verb != "has") {
		return "", false
	}
	who := w.path(&Path{Segs: p.Segs[:len(p.Segs)-1]})
	switch {
	case negated && verb == "is":
		verb = "is not"
	case negated:
		verb = "does not have"
	}
	return who + " " + verb + " " + rest, true
}

var comparisons = map[string]string{
	"=": "is", "!=": "is not", "<": "is less than", "<=": "is at most", ">": "is more than", ">=": "is at least", "in": "is one of",
}

func (w *sayer) compare(e *Binary) string {
	if say, ok := comparisons[e.Op]; ok {
		return w.value(e.L) + " " + say + " " + w.value(e.R)
	}
	return w.value(e.L) + " " + e.Op + " " + w.value(e.R)
}

func (w *sayer) value(e Expr) string {
	switch e := e.(type) {
	case *Path:
		return w.path(e)
	case *Ident:
		return splitName(e.Name)
	case *NoneLit:
		return "nothing"
	case *Set:
		parts := make([]string, len(e.Elems))
		for i, x := range e.Elems {
			parts[i] = w.value(x)
		}
		return strings.Join(parts, ", ")
	case *Binary:
		switch e.Op {
		case "+", "-":
			op := " plus "
			if e.Op == "-" {
				op = " minus "
			}
			return w.value(e.L) + op + w.value(e.R)
		}
		return "(" + w.cond(e) + ")"
	case *Not:
		return "(" + w.cond(e) + ")"
	case *Scale:
		return fmt.Sprintf("%d times %s", e.K, w.value(e.X))
	case *Call:
		switch {
		case e.Fn == "len" && len(e.Args) == 1:
			return "the length of " + w.value(e.Args[0])
		case e.Fn == "old" && len(e.Args) == 1:
			return w.value(e.Args[0]) + " before the step"
		}
		if s, ok := w.call(e); ok {
			return "(" + s + ")"
		}
		args := make([]string, len(e.Args))
		for i, x := range e.Args {
			args[i] = w.value(x)
		}
		return e.Fn + "(" + strings.Join(args, ", ") + ")"
	case *Index:
		return w.value(e.X) + " for " + w.value(e.K)
	case *Quant:
		coll, elem := w.collection(e)
		inner := w.with(e.Var, elem)
		switch e.Op {
		case "count":
			return "the number of " + coll + " where " + inner.cond(e.Body)
		case "any":
			return "(at least one of " + coll + " is such that " + inner.cond(e.Body) + ")"
		case "all":
			return "(every one of " + coll + " is such that " + inner.cond(e.Body) + ")"
		}
	}
	return e.String()
}

// collection names what a quantifier ranges over, and what one of them is
// called. instances(E) is every other E, never self.
func (w *sayer) collection(q *Quant) (coll, elem string) {
	if p, ok := q.Coll.(*Path); ok && len(p.Segs) == 2 && p.Segs[1] == InstancesRelation {
		e := splitName(p.Segs[0])
		if strings.EqualFold(p.Segs[0], w.self) {
			return "other " + plural(e), e // never self
		}
		return plural(e), e
	}
	return w.value(q.Coll), ""
}

func (w *sayer) with(v, elem string) *sayer {
	vars := map[string]string{}
	for k, x := range w.vars {
		vars[k] = x
	}
	vars[v] = elem
	return &sayer{self: w.self, vars: vars}
}

func (w *sayer) path(p *Path) string {
	if len(p.Segs) == 2 && p.Segs[1] == InstancesRelation {
		return "every other " + splitName(p.Segs[0])
	}
	owner, rest := p.Segs[0], p.Segs[1:]
	var who string
	switch elem, ok := w.vars[owner]; {
	case ok && elem != "":
		who = "that " + elem
	case ok:
		who = "that one"
	case owner == "self" && w.self != "":
		who = "the " + splitName(w.self)
	case owner == "self":
		who = "it"
	case owner == "request":
		who = "the request"
	default:
		who = "the " + splitName(owner)
	}
	if len(rest) == 0 {
		return who
	}
	parts := make([]string, len(rest))
	for i, s := range rest {
		parts[i] = splitName(s)
	}
	return who + "'s " + strings.Join(parts, "'s ")
}

func splitName(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1])) {
			b.WriteRune(' ')
		}
		if r == '_' {
			r = ' '
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func article(s string) string {
	if s != "" && strings.ContainsRune("aeiou", rune(s[0])) {
		return "an " + s
	}
	return "a " + s
}

func plural(s string) string {
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	}
	return s + "s"
}

func capital(s string) string {
	for i, r := range s {
		if unicode.IsLetter(r) {
			return s[:i] + string(unicode.ToUpper(r)) + s[i+len(string(r)):]
		}
	}
	return s
}
