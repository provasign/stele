package checker

import (
	"context"
	"fmt"
	"strings"

	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// sliceState is off only in tests that compare the sliced state check with
// the whole one.
var sliceState = true

// sliced checks the state unit as the parts that share no variable
// (docs/06-checker.md#slicing), each answered from the cache on its own, so
// a changed invariant re-solves only its part.
//
// The state unit has no frame and no default: its question is its rules,
// assumptions and variable ranges alone. Parts that share no variable are
// independent: a situation with no response exists exactly when one exists
// in some part, each minimal conflict lies in one part, and a rule's
// condition or a response value is ruled out exactly when its own part rules
// it out, provided every part can hold on its own. A part that cannot (or
// that the solver cannot decide) would rule out every situation and value of
// the whole unit, so then the unit is checked whole.
func (ck *checkRun) sliced(ctx context.Context, u *lower.Unit) (UnitResult, error) {
	parts := split(u)
	if len(parts) < 2 {
		return ck.unit(ctx, u, false)
	}
	var out UnitResult
	for _, p := range parts {
		r, err := ck.unit(ctx, p, true)
		if err != nil {
			return UnitResult{}, err
		}
		if r.Whole {
			return ck.unit(ctx, u, false)
		}
		out.Conflicts = append(out.Conflicts, r.Conflicts...)
		out.Dead = append(out.Dead, r.Dead...)
		out.Vacuous = append(out.Vacuous, r.Vacuous...)
		out.Unreachable = append(out.Unreachable, r.Unreachable...)
		out.Undecided = out.Undecided || r.Undecided
		out.NearLimit = out.NearLimit || r.NearLimit
	}
	return out, nil
}

// split partitions a unit without fixed constraints into parts whose
// formulas and variables share no variable with another part. Every
// variable is in exactly one part, so a range no value satisfies is still
// seen. Order within a part follows the unit's.
func split(u *lower.Unit) []*lower.Unit {
	if len(u.Fixed()) > 0 {
		return []*lower.Unit{u}
	}
	parent := make([]int, len(u.Vars))
	index := map[string]int{}
	for i, v := range u.Vars {
		parent[i] = i
		index[v.Name] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	join := func(f *lower.Formula) bool {
		first := -1
		for _, v := range f.Vars {
			i, ok := index[v.Name]
			if !ok {
				return false
			}
			if first < 0 {
				first = i
			} else {
				parent[find(i)] = find(first)
			}
		}
		return true
	}
	groups := [][]*lower.Formula{u.Rules, u.Assumptions, u.Possible}
	for _, g := range groups {
		for _, f := range g {
			if !join(f) {
				return []*lower.Unit{u} // a formula over a variable the unit does not list
			}
		}
	}
	byRoot := map[int]*lower.Unit{}
	var parts []*lower.Unit
	partOf := func(i int) *lower.Unit {
		r := find(i)
		p := byRoot[r]
		if p == nil {
			p = &lower.Unit{Name: u.Name, Op: u.Op}
			byRoot[r] = p
			parts = append(parts, p)
		}
		return p
	}
	for i, v := range u.Vars {
		p := partOf(i)
		p.Vars = append(p.Vars, v)
	}
	// A formula over no variable at all holds or fails everywhere: it goes
	// with the first part, which every check sees.
	home := func(f *lower.Formula) *lower.Unit {
		if len(f.Vars) == 0 {
			if len(parts) == 0 {
				return nil
			}
			return parts[0]
		}
		return partOf(index[f.Vars[0].Name])
	}
	for _, f := range u.Rules {
		if p := home(f); p != nil {
			p.Rules = append(p.Rules, f)
		} else {
			return []*lower.Unit{u}
		}
	}
	for _, f := range u.Assumptions {
		if p := home(f); p != nil {
			p.Assumptions = append(p.Assumptions, f)
		} else {
			return []*lower.Unit{u}
		}
	}
	for _, f := range u.Possible {
		if p := home(f); p != nil {
			p.Possible = append(p.Possible, f)
		} else {
			return []*lower.Unit{u}
		}
	}
	return parts
}

// satisfiable asks whether the unit's assumptions, ranges and rules can all
// hold together.
func (c *q) satisfiable(ctx context.Context) (solver.Status, error) {
	var b strings.Builder
	c.preamble(&b)
	declare(&b, c.env)
	declare(&b, c.sys)
	c.assumptions(&b)
	for _, f := range c.u.Rules {
		fmt.Fprintf(&b, "(assert %s)\n", f.Term)
	}
	b.WriteString("(check-sat)\n")
	r, err := c.z.Run(ctx, b.String())
	if err != nil {
		return "", err
	}
	return r.Status, nil
}
