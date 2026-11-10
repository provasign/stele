package change

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/formalize"
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
		for _, a := range formalize.Asserted(reg, withoutOld(assert), subject) {
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
	stateInvariantWrites(reg, p, cands, out)
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// stateInvariantWrites: a state invariant that ties an attribute this
// change proposes to state existing steps change ("a lent tool is due 14
// days after it was lent") is kept by those steps, so each writes the new
// attribute. Unwritten, the step that lends could not set the due date, and
// the new rule read as clashing with lending (a developer trial,
// 2026-11-03). Only proposed attributes: an existing one a step leaves alone
// is the step's to leave.
func stateInvariantWrites(reg *ir.Registry, p vocab.Proposal, cands []translate.Candidate, out map[string][]string) {
	proposed := map[string]bool{}
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		for _, a := range e.Attributes {
			proposed[e.Name+"."+a.Name] = true
		}
	}
	for _, c := range cands {
		if c.Outcome != "rule" || c.Error != "" || c.On != "" || c.Kind != ir.Invariant || c.Assert == "" {
			continue
		}
		subject, err := reg.SubjectOf(&ir.Rule{ID: c.RuleID, Subject: c.Subject, Kind: c.Kind})
		if err != nil {
			continue
		}
		var fresh, known []*ir.Attr
		for _, src := range []string{c.When, c.Assert} {
			e, err := ir.ParseExpr(src)
			if src == "" || err != nil {
				continue
			}
			for _, a := range reg.Reads(e, subject) {
				switch {
				case a == nil || a.WorldSet || a.Entity == nil:
				case proposed[a.QName()]:
					fresh = append(fresh, a)
				default:
					known = append(known, a)
				}
			}
		}
		// What the invariant ties may itself be new: at an adoption every
		// attribute is. "A receipt lists the due date" against a step that
		// prints the receipt then clashed, the receipt's kind written and
		// what it lists held fixed, and came to the owner three times (a first adoption, 2026-11-10). A step that
		// writes one attribute the invariant reads writes the new ones it
		// ties to it on the same entity.
		for _, op := range reg.Operations {
			var written []*ir.Attr
			for _, a := range append(append([]*ir.Attr{}, known...), fresh...) {
				if op.WritesAttr(a) {
					written = append(written, a)
				}
			}
			for _, a := range fresh {
				if op.WritesAttr(a) || slices.Contains(out[op.Name], a.Path()) {
					continue
				}
				if slices.ContainsFunc(written, func(w *ir.Attr) bool { return w.Entity == a.Entity }) {
					out[op.Name] = append(out[op.Name], a.Path())
				}
			}
		}
	}
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
func writeWrites(path string, by, kept map[string][]string) error {
	if len(by) == 0 && len(kept) == 0 {
		return removeIfExists(path)
	}
	var b strings.Builder
	b.WriteString("# Writes the change's rules need from steps the registry already declares, and values\n# steps were recorded as writing that they keep (`stele change`).\n")
	ops := slices.Sorted(maps.Keys(by))
	for _, o := range slices.Sorted(maps.Keys(kept)) {
		if !slices.Contains(ops, o) {
			ops = append(ops, o)
		}
	}
	sort.Strings(ops)
	for _, o := range ops {
		fmt.Fprintf(&b, "---\nextend: %s\n", o)
		if len(by[o]) > 0 {
			fmt.Fprintf(&b, "writes: [%s]\n", strings.Join(by[o], ", "))
		}
		if len(kept[o]) > 0 {
			fmt.Fprintf(&b, "keeps: [%s]\n", strings.Join(kept[o], ", "))
		}
	}
	return writeFile(path, b.String())
}

// reportedWrites finds, among the steps this check meets (the change's
// rules are on them, or a clash or an undecided check is in them), what
// each is recorded as writing that its approved rules only report. Approved
// registries kept such writes after write inference learned to tell
// reports from writes, and nothing corrected them (ISSUES 90): the agent is
// asked, and an answer that the step keeps the value records it in the
// change.
func reportedWrites(head *ir.Registry, res *Result) map[string][]string {
	met := map[string]bool{}
	for _, ids := range res.Rules {
		for _, id := range ids {
			if x := head.Rule(id); x != nil && x.On != "" {
				met[x.On] = true
			}
		}
	}
	if c := res.Gate.Check; c != nil {
		for _, cf := range append(slices.Clone(c.Conflicts), c.Unreachable...) {
			met[cf.Unit] = true
		}
		for _, u := range c.Undecided {
			met[u] = true
		}
	}
	out := map[string][]string{}
	for name := range met {
		op := head.Operations[name]
		if op == nil {
			continue
		}
		for _, a := range formalize.OnlyReported(head, op) {
			out[name] = append(out[name], a.Path())
		}
	}
	return out
}

// keeps are the answers that say a step keeps a value it was recorded as
// writing: operation to Entity.attribute.
func keeps(as []Answer) map[string][]string {
	out := map[string][]string{}
	for _, a := range as {
		if a.Kind == "reported_write" && a.Choice == "keeps" && !slices.Contains(out[a.Term], a.Existing) {
			out[a.Term] = append(out[a.Term], a.Existing)
		}
	}
	for _, ws := range out {
		sort.Strings(ws)
	}
	return out
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
