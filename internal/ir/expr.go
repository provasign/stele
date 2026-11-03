package ir

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Expr is a parsed IR expression (docs/05-ir-and-semantics.md#expression-grammar).
type Expr interface{ String() string }

// Binary is a binary operator: implies, or, and, a comparison, + or -.
type Binary struct {
	Op   string
	L, R Expr
}

// Not is logical negation.
type Not struct{ X Expr }

// Scale is a linear term: X * K for an integer constant K.
type Scale struct {
	X Expr
	K int
}

// Path is a dotted reference such as self.node.vmExists or request.event.
type Path struct{ Segs []string }

// Ident is a bare identifier: an enum literal such as running.
type Ident struct{ Name string }

// IntLit is an integer literal.
type IntLit struct{ V int }

// BoolLit is true or false.
type BoolLit struct{ V bool }

// Set is a set literal of enum values or integers, used with `in`.
type Set struct{ Elems []Expr }

// StrLit is a string literal, written in double quotes.
type StrLit struct{ V string }

// DecLit is a decimal literal such as 1.5, kept as written.
type DecLit struct{ Text string }

// DurLit is a duration literal (500ms, 30s, 15m, 2h, 7d, 1w), in milliseconds.
type DurLit struct {
	MS   int
	Text string
}

// TimeLit is a date (2025-01-01) or an RFC 3339 time, in milliseconds since
// the Unix epoch, UTC.
type TimeLit struct {
	MS   int
	Text string
}

// ListLit is a list literal: ["a", "b"], [] for the empty list.
type ListLit struct{ Elems []Expr }

// NoneLit is `none`: an optional attribute with no value.
type NoneLit struct{}

// Call is a built-in function: startsWith, endsWith, contains, len, isSet, has.
type Call struct {
	Fn   string
	Args []Expr
}

// Index looks up a map attribute by key: self.limits[gold].
type Index struct{ X, K Expr }

// Quant ranges a variable over a collection: all, any (true for at least one
// element) or count. Coll is a list or set attribute, or a relation to many.
type Quant struct {
	Op   string // all | any | count
	Var  string
	Coll Expr
	Body Expr
}

func (e *Binary) String() string { return "(" + e.L.String() + " " + e.Op + " " + e.R.String() + ")" }
func (e *Not) String() string    { return "not " + e.X.String() }
func (e *Scale) String() string  { return e.X.String() + " * " + strconv.Itoa(e.K) }
func (e *Path) String() string {
	if len(e.Segs) == 2 && e.Segs[1] == InstancesRelation && e.Segs[0] != "" {
		return "instances(" + strings.ToUpper(e.Segs[0][:1]) + e.Segs[0][1:] + ")"
	}
	return strings.Join(e.Segs, ".")
}
func (e *Ident) String() string   { return e.Name }
func (e *IntLit) String() string  { return strconv.Itoa(e.V) }
func (e *BoolLit) String() string { return strconv.FormatBool(e.V) }
func (e *StrLit) String() string  { return strconv.Quote(e.V) }
func (e *DecLit) String() string  { return e.Text }
func (e *DurLit) String() string  { return e.Text }
func (e *TimeLit) String() string { return e.Text }
func (e *NoneLit) String() string { return "none" }
func (e *ListLit) String() string {
	parts := make([]string, len(e.Elems))
	for i, x := range e.Elems {
		parts[i] = x.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
func (e *Index) String() string { return e.X.String() + "[" + e.K.String() + "]" }
func (e *Call) String() string {
	parts := make([]string, len(e.Args))
	for i, x := range e.Args {
		parts[i] = x.String()
	}
	return e.Fn + "(" + strings.Join(parts, ", ") + ")"
}
func (e *Quant) String() string {
	return e.Op + "(" + e.Var + " in " + e.Coll.String() + ": " + e.Body.String() + ")"
}
func (e *Set) String() string {
	parts := make([]string, len(e.Elems))
	for i, x := range e.Elems {
		parts[i] = x.String()
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// Paths returns every path in e that names registry state, in order of
// appearance. Paths rooted at a quantifier's variable are left out; the
// collection a quantifier ranges over is included, and may end at a relation
// to many (see Registry.ResolveRef).
func Paths(e Expr) []*Path {
	var out []*Path
	var walk func(Expr, map[string]bool)
	walk = func(e Expr, bound map[string]bool) {
		switch e := e.(type) {
		case *Binary:
			walk(e.L, bound)
			walk(e.R, bound)
		case *Not:
			walk(e.X, bound)
		case *Scale:
			walk(e.X, bound)
		case *Set:
			for _, x := range e.Elems {
				walk(x, bound)
			}
		case *ListLit:
			for _, x := range e.Elems {
				walk(x, bound)
			}
		case *Call:
			if HistoryFunctions[e.Fn] {
				return // the argument names an operation, not state
			}
			for _, x := range e.Args {
				walk(x, bound)
			}
		case *Index:
			walk(e.X, bound)
			walk(e.K, bound)
		case *Quant:
			walk(e.Coll, bound)
			inner := map[string]bool{e.Var: true}
			for k := range bound {
				inner[k] = true
			}
			walk(e.Body, inner)
		case *Path:
			if !bound[e.Segs[0]] {
				out = append(out, e)
			}
		}
	}
	walk(e, map[string]bool{})
	return out
}

// Functions are the built-in functions and how many arguments each takes.
var Functions = map[string]int{"startsWith": 2, "endsWith": 2, "contains": 2, "len": 1, "isSet": 1, "has": 2,
	"matches": 2, "old": 1, "ran": 1, "runs": 1, "lastRun": 1, "isEmpty": 1,
	"seconds": 1, "minutes": 1, "hours": 1, "days": 1, "min": 2, "max": 2, "abs": 1}

// InstancesRelation is the relation `instances(E)` reads: the instances of
// E other than the one roots and self name, like a relation to many. The
// loader adds it to E when a rule uses it; it is never written to files.
const InstancesRelation = "*instances"

// HistoryFunctions read an operation's history (D-051); their argument is
// an operation name, Entity.operation.
var HistoryFunctions = map[string]bool{"ran": true, "runs": true, "lastRun": true}

// quantifiers maps quantifier names to their canonical form; exists and
// forall are the names docs/05 used first.
var quantifiers = map[string]string{"all": "all", "any": "any", "count": "count", "forall": "all", "exists": "any"}

// Roots that are always paths, even without a dot (docs/05: self, actor, request, it).
var builtinRoots = map[string]bool{"self": true, "actor": true, "request": true, "result": true, "it": true}

var cmpOps = map[string]bool{"=": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "in": true}

// ParseExpr parses one IR expression (docs/05-ir-and-semantics.md#expression-grammar).
func ParseExpr(src string) (Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	e, err := p.implies()
	if err != nil {
		return nil, err
	}
	if p.peek() != "" {
		return nil, fmt.Errorf("unexpected %q after expression", p.peek())
	}
	return e, nil
}

type parser struct {
	toks []string
	pos  int
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *parser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) expect(t string) error {
	if got := p.next(); got != t {
		if got == "" {
			got = "end of expression"
		}
		return fmt.Errorf("expected %q, got %q", t, got)
	}
	return nil
}

// implies := or ('implies' implies)?   (right-associative, lowest precedence)
func (p *parser) implies() (Expr, error) {
	l, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.peek() == "implies" {
		p.next()
		r, err := p.implies()
		if err != nil {
			return nil, err
		}
		return &Binary{Op: "implies", L: l, R: r}, nil
	}
	return l, nil
}

func (p *parser) or() (Expr, error)  { return p.leftAssoc("or", p.and) }
func (p *parser) and() (Expr, error) { return p.leftAssoc("and", p.not) }

func (p *parser) leftAssoc(op string, sub func() (Expr, error)) (Expr, error) {
	l, err := sub()
	if err != nil {
		return nil, err
	}
	for p.peek() == op {
		p.next()
		r, err := sub()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: op, L: l, R: r}
	}
	return l, nil
}

func (p *parser) not() (Expr, error) {
	if p.peek() == "not" {
		p.next()
		x, err := p.not()
		if err != nil {
			return nil, err
		}
		return &Not{X: x}, nil
	}
	return p.cmp()
}

func (p *parser) cmp() (Expr, error) {
	l, err := p.arith()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); cmpOps[t] {
		p.next()
		r, err := p.arith()
		if err != nil {
			return nil, err
		}
		return &Binary{Op: t, L: l, R: r}, nil
	}
	if p.peek() == "contains" { // infix form of contains(a, b)
		p.next()
		r, err := p.arith()
		if err != nil {
			return nil, err
		}
		return &Call{Fn: "contains", Args: []Expr{l, r}}, nil
	}
	return l, nil
}

func (p *parser) arith() (Expr, error) {
	l, err := p.term()
	if err != nil {
		return nil, err
	}
	for t := p.peek(); t == "+" || t == "-"; t = p.peek() {
		p.next()
		r, err := p.term()
		if err != nil {
			return nil, err
		}
		l = &Binary{Op: t, L: l, R: r}
	}
	return l, nil
}

func (p *parser) term() (Expr, error) {
	x, err := p.postfix()
	if err != nil {
		return nil, err
	}
	if p.peek() == "*" {
		p.next()
		k, err := strconv.Atoi(p.next())
		if err != nil {
			return nil, fmt.Errorf("`*` needs an integer constant on the right (linear arithmetic only)")
		}
		return &Scale{X: x, K: k}, nil
	}
	return x, nil
}

// postfix := atom ('[' implies ']')*
func (p *parser) postfix() (Expr, error) {
	x, err := p.atom()
	if err != nil {
		return nil, err
	}
	for p.peek() == "[" {
		p.next()
		k, err := p.implies()
		if err != nil {
			return nil, err
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		x = &Index{X: x, K: k}
	}
	return x, nil
}

func (p *parser) atom() (Expr, error) {
	t := p.next()
	if t != "" && t[0] == '"' {
		v, err := strconv.Unquote(t)
		if err != nil {
			v, err = unquoteRegex(t)
		}
		if err != nil {
			return nil, fmt.Errorf("bad string %s: %v", t, err)
		}
		return &StrLit{V: v}, nil
	}
	if q, ok := quantifiers[t]; ok && p.peek() == "(" {
		return p.quant(q)
	}
	if t == "instances" && p.peek() == "(" {
		// instances(Account): every other instance of the entity.
		p.next()
		name := p.next()
		if name == "" || !isIdentStart(rune(name[0])) || strings.Contains(name, ".") {
			return nil, fmt.Errorf("instances takes an entity name: instances(Account)")
		}
		if err := p.expect(")"); err != nil {
			return nil, err
		}
		return &Path{Segs: []string{strings.ToLower(name[:1]) + name[1:], InstancesRelation}}, nil
	}
	if n, ok := Functions[t]; ok && p.peek() == "(" {
		return p.call(t, n)
	}
	switch {
	case t == "":
		return nil, fmt.Errorf("unexpected end of expression")
	case t == "(":
		e, err := p.implies()
		if err != nil {
			return nil, err
		}
		return e, p.expect(")")
	case t == "[":
		list := &ListLit{}
		for p.peek() != "]" {
			x, err := p.implies()
			if err != nil {
				return nil, err
			}
			list.Elems = append(list.Elems, x)
			if p.peek() != "," {
				break
			}
			p.next()
		}
		return list, p.expect("]")
	case t == "{":
		set := &Set{}
		for {
			e, err := p.atom()
			if err != nil {
				return nil, err
			}
			set.Elems = append(set.Elems, e)
			if p.peek() == "," {
				p.next()
				continue
			}
			return set, p.expect("}")
		}
	case t == "true" || t == "false":
		return &BoolLit{V: t == "true"}, nil
	case t == "none":
		return &NoneLit{}, nil
	case t[0] == '-' || unicode.IsDigit(rune(t[0])):
		return number(t)
	case isIdentStart(rune(t[0])):
		if strings.Contains(t, ".") || builtinRoots[t] {
			return &Path{Segs: strings.Split(t, ".")}, nil
		}
		return &Ident{Name: t}, nil
	}
	return nil, fmt.Errorf("unexpected %q", t)
}

func (p *parser) call(fn string, n int) (Expr, error) {
	p.next() // (
	c := &Call{Fn: fn}
	for p.peek() != ")" {
		x, err := p.implies()
		if err != nil {
			return nil, err
		}
		c.Args = append(c.Args, x)
		if p.peek() != "," {
			break
		}
		p.next()
	}
	if err := p.expect(")"); err != nil {
		return nil, err
	}
	if len(c.Args) != n {
		return nil, fmt.Errorf("%s takes %d argument(s), got %d", fn, n, len(c.Args))
	}
	return c, nil
}

// quant := op '(' ident 'in' postfix ':' implies ')'
func (p *parser) quant(op string) (Expr, error) {
	p.next() // (
	v := p.next()
	if v == "" || !isIdentStart(rune(v[0])) || strings.Contains(v, ".") || builtinRoots[v] {
		return nil, fmt.Errorf("%s needs a variable name: %s(x in collection: condition)", op, op)
	}
	if err := p.expect("in"); err != nil {
		return nil, err
	}
	coll, err := p.postfix()
	if err != nil {
		return nil, err
	}
	// The body follows ":" (also "|" or ",", which YAML accepts unquoted).
	if sep := p.next(); sep != ":" && sep != "|" && sep != "," {
		return nil, fmt.Errorf("%s: expected \":\" before the condition, got %q", op, sep)
	}
	body, err := p.implies()
	if err != nil {
		return nil, err
	}
	return &Quant{Op: op, Var: v, Coll: coll, Body: body}, p.expect(")")
}

var units = map[string]int{"ms": 1, "s": 1000, "m": 60 * 1000, "h": 60 * 60 * 1000, "d": 24 * 60 * 60 * 1000, "w": 7 * 24 * 60 * 60 * 1000}

// number parses an integer, a decimal, a duration or a date/time literal.
func number(t string) (Expr, error) {
	if v, err := strconv.Atoi(t); err == nil {
		return &IntLit{V: v}, nil
	}
	body := strings.TrimPrefix(t, "-")
	sign := 1
	if body != t {
		sign = -1
	}
	if _, err := strconv.ParseFloat(body, 64); err == nil && strings.Count(body, ".") == 1 && !strings.ContainsAny(body, "eE") {
		return &DecLit{Text: t}, nil
	}
	i := 0
	for i < len(body) && unicode.IsDigit(rune(body[i])) {
		i++
	}
	if n, err := strconv.Atoi(body[:i]); err == nil && i > 0 {
		if u, ok := units[body[i:]]; ok {
			return &DurLit{MS: sign * n * u, Text: t}, nil
		}
	}
	if sign == 1 {
		for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04"} {
			if tm, err := time.ParseInLocation(layout, t, time.UTC); err == nil {
				return &TimeLit{MS: int(tm.UnixMilli()), Text: t}, nil
			}
		}
	}
	return nil, fmt.Errorf("bad number %q (integers, decimals such as 1.5, durations such as 500ms 30s 15m 2h 7d 1w, dates such as 2025-01-01)", t)
}

func isIdentStart(r rune) bool { return unicode.IsLetter(r) || r == '_' }
func isIdentPart(r rune) bool  { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// lex splits src into tokens. Dotted paths are one token; a leading minus is
// part of a number only where a binary minus cannot appear.
func lex(src string) ([]string, error) {
	var toks []string
	rs := []rune(src)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, string(rs[i:j+1]))
			i = j + 1
		case strings.ContainsRune("(){},+*[]:|", r):
			toks = append(toks, string(r))
			i++
		case r == '-':
			prev := ""
			if len(toks) > 0 {
				prev = toks[len(toks)-1]
			}
			operand := prev != "" && prev != "(" && prev != "{" && prev != "," && !cmpOps[prev] &&
				prev != "+" && prev != "-" && prev != "*" && prev != "and" && prev != "or" && prev != "not" && prev != "implies"
			if !operand && i+1 < len(rs) && unicode.IsDigit(rs[i+1]) {
				j := lexNumber(rs, i+1)
				toks = append(toks, string(rs[i:j]))
				i = j
			} else {
				toks = append(toks, "-")
				i++
			}
		case r == '=':
			toks = append(toks, "=")
			i++
		case r == '!' || r == '<' || r == '>':
			if i+1 < len(rs) && rs[i+1] == '=' {
				toks = append(toks, string(rs[i:i+2]))
				i += 2
			} else if r == '!' {
				return nil, fmt.Errorf("unexpected '!' (use `not` or `!=`)")
			} else {
				toks = append(toks, string(r))
				i++
			}
		case unicode.IsDigit(r):
			j := lexNumber(rs, i)
			toks = append(toks, string(rs[i:j]))
			i = j
		case isIdentStart(r):
			j := i
			for j < len(rs) && (isIdentPart(rs[j]) || (rs[j] == '.' && j+1 < len(rs) && isIdentStart(rs[j+1]))) {
				j++
			}
			toks = append(toks, string(rs[i:j]))
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", r)
		}
	}
	return toks, nil
}

// lexNumber returns the end of the number starting at i: digits, then a
// decimal part (1.5), a unit (30s), or the rest of a date or time
// (2025-01-01, 2025-01-01T10:30:00Z), kept whole for number to parse.
func lexNumber(rs []rune, i int) int {
	j := i
	for j < len(rs) && unicode.IsDigit(rs[j]) {
		j++
	}
	switch {
	case j-i == 4 && j < len(rs) && rs[j] == '-':
		for j < len(rs) && (unicode.IsDigit(rs[j]) || strings.ContainsRune("-T:Z+.", rs[j])) {
			j++
		}
	case j+1 < len(rs) && rs[j] == '.' && unicode.IsDigit(rs[j+1]):
		j++
		for j < len(rs) && unicode.IsDigit(rs[j]) {
			j++
		}
	case j < len(rs) && isIdentStart(rs[j]):
		for j < len(rs) && isIdentPart(rs[j]) {
			j++
		}
	}
	return j
}

// unquoteRegex reads a string literal whose escapes Go does not know, such
// as a regular expression's \s, \d or \w written with one backslash (a
// model wrote matches(request.title, "\s*") and the rule failed to compile,
// 2026-11-02). Known escapes keep their meaning; an unknown one keeps its
// backslash, which is what the regular expression means by it.
func unquoteRegex(t string) (string, error) {
	if len(t) < 2 || t[0] != '"' || t[len(t)-1] != '"' {
		return "", fmt.Errorf("not a quoted string")
	}
	var b strings.Builder
	body := t[1 : len(t)-1]
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' || i+1 == len(body) {
			b.WriteByte(c)
			continue
		}
		if v, _, tail, err := strconv.UnquoteChar(body[i:], '"'); err == nil {
			b.WriteRune(v)
			i = len(body) - len(tail) - 1
			continue
		}
		b.WriteByte('\\') // unknown escape: the backslash stays
	}
	return b.String(), nil
}
