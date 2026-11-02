package formalize

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// compactAliases applies model-proposed identity aliases mechanically. Merges
// requiring a value conversion stay as review questions; they must not delete
// a symbol and force another interpretation of already accepted source prose.
func compactAliases(st *state, merges []vocab.Merge, opt Options, dir string) error {
	validationDir, err := os.MkdirTemp(dir, ".aliases-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(validationDir)
	lookup := func(reg *ir.Registry, name string) *ir.Attr {
		entity, attr, _ := strings.Cut(name, ".")
		if e := reg.Entities[entity]; e != nil {
			return e.Attrs[attr]
		}
		return nil
	}
	review := func(mg vocab.Merge, reason string) {
		st.Flags = append(st.Flags, vocab.Flag{Symbol: mg.From, Similar: mg.Into, Reason: "merge deferred for review: " + reason + "; " + mg.Reason})
		fmt.Fprintf(opt.Log, "  consolidate: kept %s and %s separate: %s\n", mg.From, mg.Into, reason)
	}
	// Judge each merge against what compiles now: a link proposed in this
	// consolidation has not been checked yet, and one that never compiled
	// must not count as broken by the merge.
	if len(merges) > 0 {
		files, err := writeVocabulary(validationDir, st.Proposal, opt)
		if err != nil {
			return err
		}
		translate.Revalidate(files, st.Cands)
		translate.Revalidate(files, st.Links)
	}
	for _, mg := range merges {
		reg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, st.Proposal)
		if err != nil {
			return err
		}
		if !proposes(st.Proposal, mg.From) && proposes(st.Proposal, mg.Into) {
			mg.From, mg.Into = mg.Into, mg.From // fold the new name into the existing one
		}
		from, into := lookup(reg, mg.From), lookup(reg, mg.Into)
		if from == nil || into == nil || from == into {
			continue
		}
		if !identityDomain(from, into) {
			review(mg, "requires an explicit value or controller mapping")
			continue
		}
		next, applied := vocab.ApplyMerges(st.Proposal, []vocab.Merge{mg})
		if len(applied) == 0 {
			continue
		} // never remove a seed-owned attribute
		if _, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, next); err != nil {
			review(mg, err.Error())
			continue
		}
		cands, err := renameAlias(reg, st.Cands, from, into)
		if err != nil {
			review(mg, err.Error())
			continue
		}
		links, err := renameAlias(reg, st.Links, from, into)
		if err != nil {
			review(mg, err.Error())
			continue
		}
		files, err := writeVocabulary(validationDir, next, opt)
		if err != nil {
			return err
		}
		translate.Revalidate(files, cands)
		translate.Revalidate(files, links)
		broken := func(before, after []translate.Candidate) string {
			for i, c := range before {
				if c.Outcome == "rule" && c.Error == "" && after[i].Error != "" {
					return after[i].RuleID + ": " + after[i].Error
				}
			}
			return ""
		}
		if why := broken(st.Cands, cands) + broken(st.Links, links); why != "" {
			review(mg, "renamed rules do not compile ("+why+"); accepted rules preserved")
			continue
		}
		st.Proposal, st.Cands, st.Links = next, cands, links
		st.Merges = append(st.Merges, applied...)
		fmt.Fprintf(opt.Log, "  consolidate: renamed %s to %s locally\n", mg.From, mg.Into)
	}
	return nil
}

func identityDomain(a, b *ir.Attr) bool {
	// The controller may differ: an attribute no step writes is the
	// environment's only because no translation listed a writer, and folding
	// it into one a step writes gives it that writer. What the world sets
	// never folds into what the system decides, or the reverse.
	if a.Kind != b.Kind || a.WorldSet != b.WorldSet {
		return false
	}
	switch a.Kind {
	case ir.Bool:
		return true
	case ir.Int:
		return a.Min == b.Min && a.Max == b.Max
	case ir.Enum:
		left, right := slices.Clone(a.Values), slices.Clone(b.Values)
		slices.Sort(left)
		slices.Sort(right)
		return len(left) > 0 && slices.Equal(left, right)
	}
	return false
}

func renameAlias(reg *ir.Registry, original []translate.Candidate, from, into *ir.Attr) ([]translate.Candidate, error) {
	out := slices.Clone(original)
	for i := range out {
		c := &out[i]
		if c.Outcome != "rule" || c.Error != "" {
			continue
		}
		// A rule that does not resolve is left as it is (ISSUES 16): one
		// bad path elsewhere must not block every merge. If it did read the
		// merged attribute, it no longer compiles afterwards, and the merge
		// is refused there.
		rule := &ir.Rule{ID: c.RuleID, Subject: c.Subject, On: c.On, Operation: c.Operation}
		var err error
		if c.When != "" {
			if rule.When, err = ir.ParseExpr(c.When); err != nil {
				continue
			}
		}
		if c.Assert != "" {
			if rule.Assert, err = ir.ParseExpr(c.Assert); err != nil {
				continue
			}
		}
		subject, err := reg.SubjectOf(rule)
		if err != nil {
			continue
		}
		bound := map[string]int{} // quantifier variables in scope
		var rewrite func(ir.Expr) (ir.Expr, bool, error)
		rewrite = func(e ir.Expr) (ir.Expr, bool, error) {
			switch e := e.(type) {
			case *ir.Path:
				if bound[e.Segs[0]] > 0 {
					return e, false, nil // through a quantifier's variable: not a single attribute
				}
				path := e
				isDefault := c.Kind == ir.Derivation && len(e.Segs) > 1 && e.Segs[len(e.Segs)-1] == "default"
				if isDefault {
					path = &ir.Path{Segs: e.Segs[:len(e.Segs)-1]}
				}
				ref, err := reg.ResolveRef(path, subject)
				if err != nil {
					return nil, false, err
				}
				if a := ref.Attr; a != from {
					return e, false, nil
				}
				name := into.Entity.Name
				segs := []string{strings.ToLower(name[:1]) + name[1:], into.Name}
				if isDefault {
					segs = append(segs, "default")
				}
				return &ir.Path{Segs: segs}, true, nil
			case *ir.Binary:
				l, lc, err := rewrite(e.L)
				if err != nil {
					return nil, false, err
				}
				r, rc, err := rewrite(e.R)
				return &ir.Binary{Op: e.Op, L: l, R: r}, lc || rc, err
			case *ir.Not:
				x, changed, err := rewrite(e.X)
				return &ir.Not{X: x}, changed, err
			case *ir.Scale:
				x, changed, err := rewrite(e.X)
				return &ir.Scale{X: x, K: e.K}, changed, err
			case *ir.Set:
				copy := &ir.Set{}
				changed := false
				for _, x := range e.Elems {
					y, yc, err := rewrite(x)
					if err != nil {
						return nil, false, err
					}
					copy.Elems = append(copy.Elems, y)
					changed = changed || yc
				}
				return copy, changed, nil
			case *ir.ListLit:
				copy := &ir.ListLit{}
				changed := false
				for _, x := range e.Elems {
					y, yc, err := rewrite(x)
					if err != nil {
						return nil, false, err
					}
					copy.Elems = append(copy.Elems, y)
					changed = changed || yc
				}
				return copy, changed, nil
			case *ir.Call:
				copy := &ir.Call{Fn: e.Fn}
				changed := false
				for _, x := range e.Args {
					y, yc, err := rewrite(x)
					if err != nil {
						return nil, false, err
					}
					copy.Args = append(copy.Args, y)
					changed = changed || yc
				}
				return copy, changed, nil
			case *ir.Index:
				x, xc, err := rewrite(e.X)
				if err != nil {
					return nil, false, err
				}
				k, kc, err := rewrite(e.K)
				return &ir.Index{X: x, K: k}, xc || kc, err
			case *ir.Quant:
				coll, cc, err := rewrite(e.Coll)
				if err != nil {
					return nil, false, err
				}
				bound[e.Var]++
				body, bc, err := rewrite(e.Body)
				bound[e.Var]--
				return &ir.Quant{Op: e.Op, Var: e.Var, Coll: coll, Body: body}, cc || bc, err
			}
			return e, false, nil
		}
		when, assert := c.When, c.Assert
		failed := false
		for _, field := range []struct {
			text *string
			expr ir.Expr
		}{{&when, rule.When}, {&assert, rule.Assert}} {
			if field.expr == nil {
				continue
			}
			e, changed, err := rewrite(field.expr)
			if err != nil {
				failed = true
				break
			}
			if changed {
				*field.text = e.String()
			}
		}
		if !failed {
			c.When, c.Assert = when, assert
		}
	}
	return out, nil
}

// proposes reports whether the proposal declares the attribute Entity.name.
func proposes(p vocab.Proposal, qname string) bool {
	ent, name, _ := strings.Cut(qname, ".")
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		if e.Name != ent {
			continue
		}
		for _, a := range e.Attributes {
			if a.Name == name {
				return true
			}
		}
	}
	return false
}
