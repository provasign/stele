// Package graph indexes a registry as a graph of statements, rules,
// attributes, entities and operations, derived from the YAML on every load
// (the YAML stays the source of truth). It answers the questions adding a
// new spec asks: which existing vocabulary a new statement is about, so a
// translation request carries only that; and how conflicts group by the
// statements they share, so a developer reads root causes, not combinations.
package graph

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/provasign/stele/internal/ir"
)

// Graph is a registry with its edges.
type Graph struct {
	Reg *ir.Registry
	// Readers maps an attribute to the rules that read it; Writers to the
	// operations that write it.
	Readers map[*ir.Attr][]*ir.Rule
	Writers map[*ir.Attr][]*ir.Operation
	// terms are each attribute's words: its entity, its name, its meaning,
	// and the prose of the statements whose rules read it.
	terms map[*ir.Attr]map[string]float64
	df    map[string]int // in how many attributes' terms a word appears
	attrs []*ir.Attr
	// ruleWords are each rule's prose words; ruleDF counts the rules a word
	// appears in.
	ruleWords map[*ir.Rule]map[string]bool
	ruleDF    map[string]int
}

// Build indexes reg.
func Build(reg *ir.Registry) *Graph {
	g := &Graph{Reg: reg, Readers: map[*ir.Attr][]*ir.Rule{}, Writers: map[*ir.Attr][]*ir.Operation{},
		terms: map[*ir.Attr]map[string]float64{}, df: map[string]int{}}
	for _, x := range reg.Rules {
		subject, _ := reg.SubjectOf(x)
		for _, e := range []ir.Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			for _, a := range reg.Reads(e, subject) {
				if !containsRule(g.Readers[a], x) {
					g.Readers[a] = append(g.Readers[a], x)
				}
			}
		}
	}
	for _, op := range reg.Operations {
		for _, a := range op.Writes {
			g.Writers[a] = append(g.Writers[a], op)
		}
	}
	g.ruleWords, g.ruleDF = map[*ir.Rule]map[string]bool{}, map[string]int{}
	for _, x := range reg.Rules {
		ws := map[string]bool{}
		for _, w := range Words(x.Prose) {
			ws[w] = true
		}
		g.ruleWords[x] = ws
		for w := range ws {
			g.ruleDF[w]++
		}
	}
	for _, name := range sortedNames(reg) {
		e := reg.Entities[name]
		for _, an := range e.AttrNames() {
			a := e.Attrs[an]
			t := map[string]float64{}
			add := func(text string, w float64) {
				for _, word := range Words(text) {
					t[word] = max(t[word], w)
				}
			}
			add(strings.ReplaceAll(e.Name, "$", " "), 1)
			add(a.Name, 2)
			add(a.Meaning, 1)
			for _, v := range a.Values {
				add(v, 1)
			}
			for _, r := range g.Readers[a] {
				add(r.Prose, 0.5)
			}
			g.terms[a] = t
			g.attrs = append(g.attrs, a)
			for word := range t {
				g.df[word]++
			}
		}
	}
	return g
}

func containsRule(rs []*ir.Rule, x *ir.Rule) bool {
	for _, r := range rs {
		if r == x {
			return true
		}
	}
	return false
}

func sortedNames(reg *ir.Registry) []string {
	out := make([]string, 0, len(reg.Entities))
	for n := range reg.Entities {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Words splits text into lower-case words, breaking identifiers at case
// changes and digits and dropping a plural s: "followEnabled" gives
// "follow", "enabled".
func Words(text string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) >= 3 {
			w := strings.ToLower(string(cur))
			if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is") {
				w = w[:len(w)-1]
			}
			if !stop[w] {
				out = append(out, w)
			}
		}
		cur = cur[:0]
	}
	rs := []rune(text)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r):
			if unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]) {
				flush()
			}
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

var stop = map[string]bool{"the": true, "and": true, "must": true, "shall": true, "should": true, "when": true, "with": true,
	"for": true, "that": true, "this": true, "not": true, "are": true, "from": true, "system": true, "any": true,
	"all": true, "into": true, "its": true, "has": true, "have": true, "will": true, "can": true, "may": true, "self": true}

// Related returns the attributes most related to text, best first: those
// whose words it shares, weighted by how rare each word is.
func (g *Graph) Related(text string, k int) []*ir.Attr {
	var out []*ir.Attr
	for _, r := range g.scoreAttrs(text) {
		if len(out) == k {
			break
		}
		out = append(out, r.a)
	}
	return out
}

type scoredAttr struct {
	a *ir.Attr
	s float64
}

// scoreAttrs scores every attribute sharing a word with text, best first.
func (g *Graph) scoreAttrs(text string) []scoredAttr {
	words := map[string]bool{}
	for _, w := range Words(text) {
		words[w] = true
	}
	var ranked []scoredAttr
	n := float64(len(g.attrs) + 1)
	for _, a := range g.attrs {
		s := 0.0
		for w, weight := range g.terms[a] {
			if words[w] {
				s += weight * idf(n, g.df[w])
			}
		}
		if s > 0 {
			ranked = append(ranked, scoredAttr{a, s})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].s > ranked[j].s })
	return ranked
}

// RuleMatch is an approved rule related to some text.
type RuleMatch struct {
	Rule *ir.Rule
	// Words is how much of the rule's own prose the text shares, each shared
	// word weighted by how rare it is among the rules.
	Words float64
	// Named: the rule is about an operation the text names.
	Named bool
	// About: the rule is about an entity the text names ("a rental").
	About bool
}

// Constraints returns the approved rules most related to text, best first,
// for a request about to be planned: rules about an operation the text
// names (its entity and its verb both appear), then the others by how much
// of their prose the text shares, rarer words weighing more. A rule takes
// part only when it is about a named operation or entity ("a rental"), or
// its shared words weigh at least minRare times the
// weight of a word only one rule has (about minRare rare words), so text
// about nothing the registry specifies gets none. ops are the named
// operations, each with the count of its rules.
func (g *Graph) Constraints(text string, k int, minRare float64) (matches []RuleMatch, ops map[string]int) {
	words := map[string]bool{}
	for _, w := range Words(text) {
		words[w] = true
	}
	ops = map[string]int{}
	for name := range g.Reg.Operations {
		if names(words, name) {
			ops[name] = 0
		}
	}
	// Rules about an entity the text names: their subject, or an attribute
	// they read, belongs to it.
	about := map[*ir.Rule]bool{}
	for name, e := range g.Reg.Entities {
		if name == "Actor" || name == "Request" || name == "Result" || !names(words, strings.ReplaceAll(name, "$", " ")) {
			continue
		}
		for _, a := range e.Attrs {
			for _, x := range g.Readers[a] {
				about[x] = true
			}
		}
		for _, x := range g.Reg.Rules {
			if subject, err := g.Reg.SubjectOf(x); err == nil && subject != nil && subject.Name == name {
				about[x] = true
			}
		}
	}
	n := float64(len(g.Reg.Rules) + 1)
	minWords := minRare * math.Log(n)
	for _, x := range g.Reg.Rules {
		m := RuleMatch{Rule: x}
		for w := range g.ruleWords[x] {
			if words[w] {
				m.Words += math.Log(n / float64(g.ruleDF[w]))
			}
		}
		for op := range ops {
			if x.AppliesTo(op) {
				m.Named = true
				ops[op]++
			}
		}
		m.About = about[x]
		if m.Named || m.About || m.Words >= minWords {
			matches = append(matches, m)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Named != b.Named {
			return a.Named
		}
		if a.Words != b.Words {
			return a.Words > b.Words
		}
		return a.Rule.ID < b.Rule.ID
	})
	if len(matches) > k {
		matches = matches[:k]
	}
	return matches, ops
}

// names reports whether words name the operation: each word of its entity
// and its verb appears, allowing an inflection ("stopping", "stopped").
func names(words map[string]bool, op string) bool {
	ws := Words(strings.ReplaceAll(op, ".", " "))
	if len(ws) == 0 {
		return false
	}
	for _, w := range ws {
		found := false
		for t := range words {
			stem := strings.TrimSuffix(w, "e")
			if t == w || (strings.HasPrefix(t, stem) && len(t)-len(stem) <= 4) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func idf(n float64, df int) float64 {
	// Smoothed: a word every attribute has still counts a little.
	return 1 + math.Log(n/float64(max(df, 1)))
}

// Subset is the part of the registry a translation needs for attrs: their
// entities with only those attributes, the records and relations among
// those entities, the operations that write them or act on those entities,
// and the world facts (links) among them. Rules translated from statements
// are left out: a new statement is translated against the vocabulary, not
// against other statements' rules.
func (g *Graph) Subset(attrs []*ir.Attr) *ir.Registry {
	keep := map[*ir.Attr]bool{}
	ents := map[string]bool{}
	for _, a := range attrs {
		keep[a] = true
		ents[a.Entity.Name] = true
		// A record field keeps its owner, and the owner keeps the record.
		owner := a.Entity.Name
		for strings.Contains(owner, "$") {
			owner = owner[:strings.LastIndex(owner, "$")]
			ents[owner] = true
		}
	}
	out := &ir.Registry{Contexts: g.Reg.Contexts, Entities: map[string]*ir.Entity{}, Operations: map[string]*ir.Operation{}}
	for name, e := range g.Reg.Entities {
		if !ents[name] && name != "Actor" && name != "Request" && name != "Result" {
			continue
		}
		c := &ir.Entity{Name: e.Name, Context: e.Context, Attrs: map[string]*ir.Attr{}, Relations: map[string]*ir.Relation{}}
		for an, a := range e.Attrs {
			if keep[a] || name == "Actor" || name == "Request" || name == "Result" {
				c.Attrs[an] = a
			}
		}
		for rn, r := range e.Relations {
			if ents[r.To] {
				c.Relations[rn] = r
			}
		}
		out.Entities[name] = c
	}
	for name, op := range g.Reg.Operations {
		touch := ents[op.Entity.Name]
		for _, a := range op.Writes {
			touch = touch || keep[a]
		}
		if touch {
			out.Operations[name] = op
		}
	}
	for _, x := range g.Reg.Rules {
		if !strings.HasPrefix(x.ID, "link-") {
			continue
		}
		subject, _ := g.Reg.SubjectOf(x)
		for _, e := range []ir.Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			for _, a := range g.Reg.Reads(e, subject) {
				if keep[a] && !containsRule(out.Rules, x) {
					out.Rules = append(out.Rules, x)
				}
			}
		}
	}
	return out
}

// Group is conflicts sharing a root statement, the one most of them cite.
type Group struct {
	Root      string   // the statement shared, or "" for a conflict alone
	Conflicts []int    // indices into the conflicts given
	Others    []string // the other statements the group's conflicts cite
}

// Groups orders conflicts (as lists of statements) by root cause: the
// statement cited by the most conflicts roots the first group, and so on.
func Groups(conflicts [][]string) []Group {
	done := make([]bool, len(conflicts))
	var out []Group
	for {
		count := map[string]int{}
		for i, c := range conflicts {
			if !done[i] {
				for _, s := range c {
					count[s]++
				}
			}
		}
		root, best := "", 1
		for s, n := range count {
			if n > best || (n == best && n > 1 && s < root) {
				root, best = s, n
			}
		}
		if root == "" {
			break
		}
		g := Group{Root: root}
		others := map[string]bool{}
		for i, c := range conflicts {
			if done[i] || !hasString(c, root) {
				continue
			}
			done[i] = true
			g.Conflicts = append(g.Conflicts, i)
			for _, s := range c {
				if s != root {
					others[s] = true
				}
			}
		}
		for s := range others {
			g.Others = append(g.Others, s)
		}
		sort.Strings(g.Others)
		out = append(out, g)
	}
	for i := range conflicts {
		if !done[i] {
			out = append(out, Group{Conflicts: []int{i}, Others: conflicts[i]})
		}
	}
	return out
}

func hasString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
