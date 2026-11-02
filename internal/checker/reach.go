package checker

import (
	"context"
	"fmt"
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
func unreachable(ctx context.Context, z *solver.Z3, prog *lower.Program, cf Conflict) (bool, error) {
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
		return false, nil
	}
	fixed := map[string]string{} // label -> raw value
	for _, v := range unit.Vars {
		raw, ok := cf.before[v.Label]
		if !ok || raw == "" || v.Post || v.Attr == nil || v.Attr.Entity == nil || v.Label != v.Attr.QName() {
			continue // only scalar state before the step
		}
		if !written[v.Attr] {
			continue // the world sets it: any value can be reached
		}
		if len(v.Attr.Entity.CreatedBy) == 0 {
			return false, nil // instances may start in any state
		}
		fixed[v.Label] = raw
	}
	if len(fixed) == 0 {
		return false, nil
	}
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
