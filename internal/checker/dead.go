package checker

import (
	"context"
	"fmt"
	"strings"

	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// DeadResponse is something the system can never do. In the state unit it is
// a value of a system-controlled enum that no state satisfying the invariants
// has; in an operation's unit it is the operation itself, which runs from no
// state. Rules that make a state or an operation impossible conflict as surely
// as rules with no common response: the system just escapes them by never
// getting there. This is the reachability check of docs/06, one step deep.
type DeadResponse struct {
	Unit     string   `json:"unit"`
	Response string   `json:"response"`
	Rules    []string `json:"rules"`
	// NeverAllowed marks an operation whose default is deny and that no rule
	// allows. It is a vocabulary choice, not a contradiction between rules:
	// the default should be allow, or the rule that allows it is missing.
	NeverAllowed bool `json:"never_allowed,omitempty"`
}

// candidates lists the responses checked in the unit.
var basicSort = map[string]bool{"Bool": true, "Int": true, "Real": true, "String": true}

func (c *q) candidates() [][2]string {
	var out [][2]string // var name, value term
	for _, v := range c.sys {
		switch {
		case v.Attr == nil && strings.HasSuffix(v.Label, ":runs"):
			out = append(out, [2]string{v.Name, "true"})
		case c.u.Op == nil && v.Attr != nil && len(v.Attr.Values) > 0 && !basicSort[v.Sort]:
			// Only the enum value itself: an optional enum's presence flag
			// (Bool) and a collection's length (Int) carry the same attribute.
			for _, val := range v.Attr.Values {
				out = append(out, [2]string{v.Name, lower.EnumConst(v.Attr, val)})
			}
		}
	}
	return out
}

func (c *q) label(name, val string) string {
	for _, v := range c.u.Vars {
		if v.Name != name {
			continue
		}
		if v.Attr == nil {
			return strings.TrimSuffix(v.Label, ":runs") + " runs"
		}
		return v.Label + " = " + Value(v, val)
	}
	return name
}

// neverAllowed reports whether name is the unit's operation running, with
// default deny and no allow rule.
func (c *q) neverAllowed(name string) bool {
	op := c.u.Op
	if op == nil || op.Default != "deny" || name != c.runsName() {
		return false
	}
	for _, f := range c.u.Rules {
		if f.Role == lower.Allow {
			return false
		}
	}
	return true
}

func (c *q) runsName() string {
	for _, v := range c.sys {
		if v.Attr == nil && strings.HasSuffix(v.Label, ":runs") {
			return v.Name
		}
	}
	return ""
}

// reachable asks whether some situation and response satisfy the rules in
// set together with response cand.
func (c *q) reachable(ctx context.Context, cand [2]string, set []int) (solver.Status, error) {
	var b strings.Builder
	c.preamble(&b)
	declare(&b, c.env)
	declare(&b, c.sys)
	c.assumptions(&b)
	fmt.Fprintf(&b, "(assert (= %s %s))\n", cand[0], cand[1])
	for _, t := range c.u.Fixed() {
		fmt.Fprintf(&b, "(assert %s)\n", t)
	}
	for _, i := range set {
		fmt.Fprintf(&b, "(assert %s) ; %s\n", c.u.Rules[i].Term, c.u.Rules[i].Rule.ID)
	}
	b.WriteString("(check-sat)\n")
	r, err := c.z.Run(ctx, b.String())
	if err != nil {
		return "", err
	}
	return r.Status, nil
}

// deadResponses finds every dead response in the unit and a minimal set of
// rules that kills it. The bool reports a solver unknown.
func (c *q) deadResponses(ctx context.Context) ([]DeadResponse, bool, error) {
	all := make([]int, len(c.u.Rules))
	for i := range all {
		all[i] = i
	}
	var out []DeadResponse
	for _, cand := range c.candidates() {
		st, err := c.reachable(ctx, cand, all)
		if err != nil {
			return nil, false, err
		}
		if st == solver.Unknown {
			return out, true, nil
		}
		if st == solver.Sat {
			continue
		}
		core := all
		if shrunk, err := c.solverCore(ctx, func(b *strings.Builder) {
			fmt.Fprintf(b, "(assert (= %s %s))\n", cand[0], cand[1])
		}, all); err != nil {
			return nil, false, err
		} else if shrunk != nil {
			if st, err := c.reachable(ctx, cand, shrunk); err != nil {
				return nil, false, err
			} else if st == solver.Unsat {
				core = shrunk
			}
		}
		for k := 0; k < len(core); {
			trial := append(append([]int{}, core[:k]...), core[k+1:]...)
			st, err := c.reachable(ctx, cand, trial)
			if err != nil {
				return nil, false, err
			}
			switch st {
			case solver.Unsat:
				core = trial
			case solver.Sat:
				k++
			default:
				return out, true, nil
			}
		}
		d := DeadResponse{Unit: c.u.Name, Response: c.label(cand[0], cand[1])}
		seen := map[string]bool{}
		for _, i := range core {
			if id := c.u.Rules[i].Rule.ID; !seen[id] {
				seen[id] = true
				d.Rules = append(d.Rules, id)
			}
		}
		// An empty core means the operation's default, the declared ranges or
		// the state it starts from rule it out with no rule involved, e.g. a
		// default-deny operation no rule allows. That is still worth saying.
		d.NeverAllowed = len(core) == 0 && c.neverAllowed(cand[0])
		out = append(out, d)
	}
	return out, false, nil
}
