package change

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// inferWrites finds what the change's rules assert about the state after a
// step the registry already declares, and that the step does not write: a
// rule about the state after a step constrains what the step changes, so
// the step writes it, unless the world sets it or it belongs to another
// entity. Formalize does this for the
// operations it proposes; a change mostly adds rules to existing ones.
// It returns operation -> attributes.
func inferWrites(reg *ir.Registry, p vocab.Proposal, cands []translate.Candidate) map[string][]string {
	proposed := map[string]bool{}
	for _, o := range p.Operations {
		proposed[o.Name] = true
	}
	out := map[string][]string{}
	for _, c := range cands {
		if c.Outcome != "rule" || c.Error != "" || c.On == "" || c.Kind != ir.Invariant || c.Assert == "" {
			continue
		}
		op := reg.Operations[c.On]
		if op == nil || proposed[op.Name] {
			continue
		}
		assert, err := ir.ParseExpr(c.Assert)
		if err != nil {
			continue
		}
		subject, err := reg.SubjectOf(&ir.Rule{ID: c.RuleID, Subject: c.Subject, On: c.On, Kind: c.Kind})
		if err != nil {
			continue
		}
		for _, a := range reg.Reads(withoutOld(assert), subject) {
			// Only the step's own entity: an attribute reached through a
			// relation is usually read to relate to (the cluster's tenant in
			// a backup's key), not changed.
			if a == nil || a.WorldSet || a.Entity != op.Entity || op.WritesAttr(a) {
				continue
			}
			if q := a.Path(); !slices.Contains(out[op.Name], q) {
				out[op.Name] = append(out[op.Name], q)
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// withoutOld replaces every old(...) by a constant: values before the step
// are not what the step writes.
func withoutOld(e ir.Expr) ir.Expr {
	switch e := e.(type) {
	case *ir.Call:
		if e.Fn == "old" {
			return &ir.BoolLit{V: true}
		}
		args := make([]ir.Expr, len(e.Args))
		for i, a := range e.Args {
			args[i] = withoutOld(a)
		}
		return &ir.Call{Fn: e.Fn, Args: args}
	case *ir.Binary:
		x := *e
		x.L, x.R = withoutOld(e.L), withoutOld(e.R)
		return &x
	case *ir.Not:
		return &ir.Not{X: withoutOld(e.X)}
	case *ir.Scale:
		x := *e
		x.X = withoutOld(e.X)
		return &x
	case *ir.Set:
		x := &ir.Set{}
		for _, y := range e.Elems {
			x.Elems = append(x.Elems, withoutOld(y))
		}
		return x
	case *ir.ListLit:
		x := &ir.ListLit{}
		for _, y := range e.Elems {
			x.Elems = append(x.Elems, withoutOld(y))
		}
		return x
	case *ir.Index:
		return &ir.Index{X: withoutOld(e.X), K: withoutOld(e.K)}
	case *ir.Quant:
		x := *e
		x.Coll, x.Body = withoutOld(e.Coll), withoutOld(e.Body)
		return &x
	}
	return e
}

// writeWrites records the writes a change adds to existing operations, or
// removes the file when it adds none.
func writeWrites(path string, by map[string][]string) error {
	if len(by) == 0 {
		return removeIfExists(path)
	}
	var b strings.Builder
	b.WriteString("# Writes the change's rules need from steps the registry already declares (`stele change`).\n")
	ops := make([]string, 0, len(by))
	for o := range by {
		ops = append(ops, o)
	}
	sort.Strings(ops)
	for _, o := range ops {
		fmt.Fprintf(&b, "---\nextend: %s\nwrites: [%s]\n", o, strings.Join(by[o], ", "))
	}
	return writeFile(path, b.String())
}

// systemSet finds attributes the change proposes as set by the world that
// a rule of the change fixes after a step (`self.exitCode = 141` on
// Pager.handleTermination): the requirement says the system sets them, so
// the rule is the better evidence than the proposal's guess. They are made
// system-set, and the step writes them. Only attributes the change itself
// proposes, and only an explicit `attribute = value`, so clocks and other
// world values the registry declares keep their meaning. It returns the
// changed proposal and operation -> attributes.
func systemSet(reg *ir.Registry, p vocab.Proposal, cands []translate.Candidate) (vocab.Proposal, map[string][]string) {
	worldAttr := map[string]bool{}
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		for _, a := range e.Attributes {
			if a.SetBy == "world" {
				worldAttr[e.Name+"."+a.Name] = true
			}
		}
	}
	out := map[string][]string{}
	for _, c := range cands {
		if c.Outcome != "rule" || c.On == "" || c.Kind != ir.Invariant || c.Assert == "" {
			continue
		}
		op := reg.Operations[c.On]
		assert, err := ir.ParseExpr(c.Assert)
		if op == nil || err != nil {
			continue
		}
		for _, name := range fixed(assert) {
			q := op.Entity.Name + "." + name
			if worldAttr[q] && !slices.Contains(out[op.Name], q) {
				out[op.Name] = append(out[op.Name], q)
			}
		}
	}
	if len(out) == 0 {
		return p, nil
	}
	flip := map[string]bool{}
	for _, qs := range out {
		for _, q := range qs {
			flip[q] = true
		}
	}
	fix := func(es []vocab.Entity) []vocab.Entity {
		res := make([]vocab.Entity, len(es))
		for i, e := range es {
			e.Attributes = slices.Clone(e.Attributes)
			for j, a := range e.Attributes {
				if flip[e.Name+"."+a.Name] {
					e.Attributes[j].SetBy = ""
				}
			}
			res[i] = e
		}
		return res
	}
	p.Entities, p.Extensions = fix(p.Entities), fix(p.Extensions)
	p.Operations = slices.Clone(p.Operations)
	for i, o := range p.Operations {
		for _, q := range out[o.Name] {
			if !slices.Contains(o.Writes, q) {
				p.Operations[i].Writes = append(slices.Clone(p.Operations[i].Writes), q)
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return p, out
}

// fixed lists the self attributes an assertion sets to a value: conjuncts
// `self.a = v` with v a literal, also on the right of an implication.
func fixed(e ir.Expr) []string {
	b, ok := e.(*ir.Binary)
	if !ok {
		return nil
	}
	switch b.Op {
	case "and":
		return append(fixed(b.L), fixed(b.R)...)
	case "implies":
		return fixed(b.R)
	case "=":
		for _, side := range [][2]ir.Expr{{b.L, b.R}, {b.R, b.L}} {
			p, ok := side[0].(*ir.Path)
			if !ok || len(p.Segs) != 2 || p.Segs[0] != "self" {
				continue
			}
			switch side[1].(type) {
			case *ir.IntLit, *ir.BoolLit, *ir.StrLit, *ir.Ident, *ir.DecLit:
				return []string{p.Segs[1]}
			}
		}
	}
	return nil
}
