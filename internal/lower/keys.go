package lower

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/ir"
)

// A uniqueness invariant, all(u in instances(E): u.k != self.k), makes k a
// key of E: two values of E with the same k are one instance, so they agree
// on everything. The instances a step reads sit in separate slots (the
// step's own, instances(E), each item of a relation to many such as
// Shed.tools), and nothing tied a slot to another: the items of Shed.tools
// could share an id with different borrowers, and a result keyed by id was
// shown contradicting itself in a state the approved uniqueness rule rules
// out (a developer trial, 2026-11-03).

var uniqueness = regexp.MustCompile(`^all\((\w+) in instances\((\w+)\): \((?:(\w+)\.(\w+) != self\.(\w+)|self\.(\w+) != (\w+)\.(\w+))\)\)$`)

// keyOf returns the key attribute a rule makes of its subject, or nil.
func (lw *lowerer) keyOf(x *ir.Rule) *ir.Attr {
	if x.Kind != ir.Invariant || x.On != "" || x.When != nil || x.Assert == nil {
		return nil
	}
	subject, err := lw.reg.SubjectOf(x)
	if err != nil || subject == nil {
		return nil
	}
	m := uniqueness.FindStringSubmatch(x.Assert.String())
	if m == nil || m[2] != subject.Name {
		return nil
	}
	v, k := m[1], ""
	switch {
	case m[3] == v && m[4] == m[5]:
		k = m[4]
	case m[7] == v && m[6] == m[8]:
		k = m[6]
	default:
		return nil
	}
	return subject.Attrs[k]
}

// keyAgreement states, for each key a live rule makes, that the slots of
// its entity among vars agree on every attribute they hold when both are
// present and have the same key. Only the state before the step is tied:
// the step changes its own instance, which then differs from what a slot
// still shows.
func (lw *lowerer) keyAgreement(live []*ir.Rule, vars map[string]*Var) []*Formula {
	var out []*Formula
	for _, x := range live {
		key := lw.keyOf(x)
		if key == nil {
			continue
		}
		// slot prefix -> part label ("id", "borrowerEmail?") -> variable
		slots := map[string]map[string]*Var{}
		for _, v := range vars {
			if v.Post || v.Attr == nil || v.Attr.Entity != key.Entity {
				continue
			}
			i := strings.LastIndex(v.Label, "."+v.Attr.Name)
			if i <= 0 {
				continue
			}
			part := v.Label[i+1+len(v.Attr.Name):]
			if part != partValue && !strings.HasPrefix(part, "?") && !strings.HasPrefix(part, "#") {
				continue
			}
			prefix := v.Label[:i]
			if slots[prefix] == nil {
				slots[prefix] = map[string]*Var{}
			}
			slots[prefix][v.Attr.Name+part] = v
		}
		var prefixes []string
		for p, parts := range slots {
			if parts[key.Name] != nil {
				prefixes = append(prefixes, p)
			}
		}
		sort.Strings(prefixes)
		for i, a := range prefixes {
			for _, b := range prefixes[i+1:] {
				used := map[string]*Var{}
				cond := []string{fmt.Sprintf("(= %s %s)", slots[a][key.Name].Name, slots[b][key.Name].Name)}
				for _, p := range []string{a, b} {
					if in, ok := lw.vars[p+":in"]; ok {
						cond = append(cond, in.Name)
						used[in.Label] = in
					}
					if set := slots[p][key.Name+partSet]; set != nil {
						cond = append(cond, set.Name)
					}
				}
				var same []string
				var labels []string
				for l := range slots[a] {
					if slots[b][l] != nil {
						labels = append(labels, l)
					}
				}
				sort.Strings(labels)
				for _, l := range labels {
					va, vb := slots[a][l], slots[b][l]
					if va.Sort != vb.Sort {
						continue
					}
					same = append(same, fmt.Sprintf("(= %s %s)", va.Name, vb.Name))
					used[va.Label], used[vb.Label] = va, vb
				}
				if len(same) <= 1 {
					continue // the key alone: nothing to tie
				}
				out = append(out, &Formula{Rule: x, Term: fmt.Sprintf("(=> %s %s)", conj(cond), conj(same)), Vars: sortedVars(used)})
			}
		}
	}
	return out
}
