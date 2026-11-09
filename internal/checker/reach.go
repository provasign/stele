package checker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// unreachable reports whether no sequence of steps enters a step conflict's
// state before (ISSUES 3). The step check assumes the state before is any
// state the invariants allow. Of the values it fixes for state some
// operation writes, call them S: a trace first has S either when an
// operation creates the instance (its entity names its creating operations;
// otherwise instances may start anywhere and S counts as reachable) or when
// a step changes the state into S from one that differs. If no creating
// step can leave S behind and no step can change into it, under every rule,
// no trace ever has S. Units are slices, so each query holds fewer
// constraints than the whole registry, which only makes S look reachable
// more often: the answer is sound.
//
// S is what the conflict's one sampled situation fixes, not every state the
// rules clash in; region is S as a term over the unit's variables, so that
// the caller can look for the clash outside it. With widen, S first loses
// each value it can lose and stay unreachable: excluded value by value, a
// clash that ignores an integer was sampled again at its every value.
func unreachable(ctx context.Context, z *solver.Z3, prog *lower.Program, cf Conflict, widen bool) (off bool, region string, err error) {
	// The values the conflict fixes for state some operation writes.
	written := map[*ir.Attr]bool{}
	for _, u := range prog.Units {
		if u.Op != nil {
			for _, a := range u.Op.Writes {
				written[a] = true
			}
		}
	}
	var unit *lower.Unit
	for _, u := range prog.Units {
		if u.Name == cf.Unit {
			unit = u
		}
	}
	if unit == nil || unit.Op == nil {
		return false, "", nil
	}
	// The instance the step acts on is one group; each other instance the
	// situation has present (instances(E) slots, "E.*instances#k.attr") is
	// another. No step acts on another instance, so it is in a state no
	// trace reaches when no step ever leaves an instance so. A clash between
	// two readings of "holds" needed another packet held by a member yet
	// available, which no step leaves, and the owner was asked whether that
	// could happen (a live trial, 2026-11-08).
	type group struct {
		fixed map[string]string // the attribute's label (E.a, E.a?) -> raw value
		names map[string]string // that label -> the unit's variable
	}
	groups := map[string]*group{}
	get := func(key string) *group {
		if groups[key] == nil {
			groups[key] = &group{fixed: map[string]string{}, names: map[string]string{}}
		}
		return groups[key]
	}
	for _, v := range unit.Vars {
		raw, ok := cf.before[v.Label]
		if !ok || raw == "" || v.Post || v.Attr == nil || v.Attr.Entity == nil {
			continue // only state before the step
		}
		key, label := "", ""
		switch q := v.Attr.QName(); {
		case v.Label == q || v.Label == q+"?":
			label = v.Label
		default:
			// Another instance: E.*instances#k.a or E.*instances#k.a?
			pre := v.Attr.Entity.Name + "." + ir.InstancesRelation + "#"
			rest, ok := strings.CutPrefix(v.Label, pre)
			if !ok {
				continue
			}
			k, attr, ok := strings.Cut(rest, ".")
			if !ok || strings.TrimSuffix(attr, "?") != v.Attr.Name {
				continue // a list or map slot: only scalar state
			}
			if cf.before[pre+k+":in"] != "true" {
				continue // not present: its values mean nothing
			}
			key, label = pre+k, q+strings.TrimPrefix(attr, v.Attr.Name)
		}
		if !written[v.Attr] {
			continue // the world sets it: any value can be reached
		}
		if label == v.Attr.QName() && cf.before[strings.TrimSuffix(v.Label, "?")+"?"] == "false" {
			continue // unset: its value means nothing
		}
		if len(v.Attr.Entity.CreatedBy) == 0 {
			// Instances may start in any state, so this value is reachable;
			// left out, the rest describes more states than the conflict's,
			// and if no trace reaches those, none reaches the conflict's.
			// Giving up here kept a clash over a tool's state because the
			// same step also wrote a loan record (a developer trial,
			// 2026-11-03).
			continue
		}
		g := get(key)
		g.fixed[label] = raw
		g.names[label] = v.Name
	}
	for _, key := range slices.Sorted(maps.Keys(groups)) {
		g := groups[key]
		if len(g.fixed) == 0 {
			continue
		}
		if off, err = neverEntered(ctx, z, prog, g.fixed); err != nil {
			return false, "", err
		} else if !off {
			continue
		}
		labels := slices.Sorted(maps.Keys(g.fixed))
		if widen {
			for _, l := range labels {
				if len(g.fixed) == 1 {
					break
				}
				raw := g.fixed[l]
				delete(g.fixed, l)
				if still, err := neverEntered(ctx, z, prog, g.fixed); err != nil {
					return false, "", err
				} else if !still {
					g.fixed[l] = raw
				}
			}
		}
		return true, everyInstance(unit, g.fixed), nil
	}
	return false, "", nil
}

// everyInstance is the region where any instance a unit holds, the one the
// step acts on or another present one, has the values fixed (labels E.a and
// E.a?): no trace reaches any of them, so a clash resampled outside the
// region cannot put the same state in the next slot.
func everyInstance(unit *lower.Unit, fixed map[string]string) string {
	byLabel := map[string]string{}
	slots := map[string]bool{}
	for _, v := range unit.Vars {
		if v.Post {
			continue
		}
		byLabel[v.Label] = v.Name
		if pre, _, ok := strings.Cut(v.Label, ":in"); ok && strings.Contains(pre, "."+ir.InstancesRelation+"#") && strings.HasSuffix(v.Label, ":in") {
			slots[pre] = true
		}
	}
	var places []string
	at := func(prefix, in string) {
		parts := []string{}
		if in != "" {
			parts = append(parts, in)
		}
		for _, l := range slices.Sorted(maps.Keys(fixed)) {
			ent, attr, _ := strings.Cut(l, ".")
			name := byLabel[l]
			if prefix != "" {
				if !strings.HasPrefix(prefix, ent+".") {
					return
				}
				name = byLabel[prefix+"."+attr]
			}
			if name == "" {
				return // this instance has no such slot here
			}
			parts = append(parts, fmt.Sprintf("(= %s %s)", name, fixed[l]))
		}
		places = append(places, conjoin(parts))
	}
	at("", "")
	for _, slot := range slices.Sorted(maps.Keys(slots)) {
		at(slot, byLabel[slot+":in"])
	}
	switch len(places) {
	case 0:
		return "false"
	case 1:
		return places[0]
	}
	return "(or " + strings.Join(places, " ") + ")"
}

// neverEntered reports whether no step leaves an instance with the values
// fixed: none creates one so, and none changes one into them.
func neverEntered(ctx context.Context, z *solver.Z3, prog *lower.Program, fixed map[string]string) (bool, error) {
	for _, u := range prog.Units {
		if u.Op == nil {
			continue
		}
		touches := false
		for _, a := range u.Op.Writes {
			_, ok := fixed[a.QName()]
			touches = touches || ok
		}
		creates := false
		for _, a := range u.Op.Writes {
			for label := range fixed {
				if ent, _, _ := strings.Cut(label, "."); a.Entity != nil && a.Entity.Name == ent && slices.Contains(a.Entity.CreatedBy, u.Op.Name) {
					creates = true
				}
			}
		}
		if !touches && !creates {
			continue // the step keeps those values: it never changes into S
		}
		var b strings.Builder
		b.WriteString("(set-logic ALL)\n")
		for _, d := range prog.Datatypes {
			b.WriteString(d + "\n")
		}
		declare(&b, u.Vars)
		for _, a := range u.Assumptions {
			fmt.Fprintf(&b, "(assert %s)\n", a.Term)
		}
		for _, t := range u.Fixed() {
			fmt.Fprintf(&b, "(assert %s)\n", t)
		}
		for _, f := range u.Rules {
			fmt.Fprintf(&b, "(assert %s)\n", f.Term)
		}
		var runs string
		before, after := map[string]*lower.Var{}, map[string]*lower.Var{}
		for _, v := range u.Vars {
			switch {
			case v.Label == u.Op.Name+":runs":
				runs = v.Name
			case v.Post:
				after[strings.TrimSuffix(v.Label, "'")] = v
			default:
				before[v.Label] = v
			}
		}
		if runs == "" {
			return false, nil // nothing constrains the step: it may write anything
		}
		fmt.Fprintf(&b, "(assert %s)\n", runs)
		var differs []string
		free := false
		for label, raw := range fixed {
			if v := after[label]; v != nil {
				fmt.Fprintf(&b, "(assert (= %s %s))\n", v.Name, raw)
			} else if w := before[label]; w != nil && !u.Op.WritesAttr(w.Attr) {
				fmt.Fprintf(&b, "(assert (= %s %s))\n", w.Name, raw) // kept by the frame
			}
			if w := before[label]; w != nil {
				differs = append(differs, fmt.Sprintf("(not (= %s %s))", w.Name, raw))
			} else {
				free = true // the unit does not read it before: it may have differed
			}
		}
		if !creates && !free {
			fmt.Fprintf(&b, "(assert (or %s))\n", strings.Join(append(differs, "false"), " "))
		}
		b.WriteString("(check-sat)\n")
		r, err := z.Run(ctx, b.String())
		if err != nil {
			return false, err
		}
		if r.Status != solver.Unsat {
			return false, nil // this step can enter S, or the solver cannot tell
		}
	}
	return true, nil
}

// CreatorsSettingAside lists the operations of the entity whose step a
// conflict is in that, recorded as the step that creates it, would set the
// conflict aside: no trace would reach its state. An entity with no
// creating step may start in any state, so its clashes stand however
// unreachable their state; the change check asks which step creates it only
// when the answer could matter. The entity must have no creators yet; the
// registry is left as it was.
func CreatorsSettingAside(ctx context.Context, z *solver.Z3, reg *ir.Registry, cf Conflict) ([]string, error) {
	op := reg.Operations[cf.Unit]
	if op == nil || op.Entity == nil || len(op.Entity.CreatedBy) > 0 || len(cf.before) == 0 {
		return nil, nil
	}
	var names []string
	for name, o := range reg.Operations {
		if o.Entity == op.Entity {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	defer func() { op.Entity.CreatedBy = nil }()
	var out []string
	for _, name := range names {
		op.Entity.CreatedBy = []string{name}
		prog, err := lower.Lower(reg, lower.Options{})
		if err != nil {
			return out, err
		}
		gone, _, err := unreachable(ctx, z, prog, cf, false)
		if err != nil {
			return out, err
		}
		if gone {
			out = append(out, name)
		}
	}
	return out, nil
}
