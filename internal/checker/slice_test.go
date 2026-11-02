package checker

import (
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/provasign/stele/internal/lower"
)

// findings names what a check found, for comparing two ways of checking.
func findings(res *Result) []string {
	out := append([]string{"verdict " + res.Verdict}, cores(res)...)
	for _, d := range res.Dead {
		out = append(out, "dead "+d.Unit+" "+d.Response)
	}
	for _, v := range res.Vacuous {
		out = append(out, "vacuous "+v.Unit+" "+v.Rule)
	}
	for _, u := range res.Undecided {
		out = append(out, "undecided "+u)
	}
	sort.Strings(out)
	return out
}

func checkSlicedAndWhole(t *testing.T, dir string, opt lower.Options) (sliced, whole []string) {
	t.Helper()
	sliced = findings(check(t, dir, opt))
	sliceState = false
	defer func() { sliceState = true }()
	whole = findings(check(t, dir, opt))
	return sliced, whole
}

// TestSlicedStateAgrees: checking the state unit in parts that share no
// variable finds what checking it whole finds, including when one part
// cannot hold on its own and every value of the unit is ruled out.
func TestSlicedStateAgrees(t *testing.T) {
	dir := filepath.Join("..", "testdata", "project-a", "registry")
	for _, opt := range []lower.Options{{IgnoreDecisions: true}, {}} {
		if s, w := checkSlicedAndWhole(t, dir, opt); !reflect.DeepEqual(s, w) {
			t.Errorf("project-a %+v:\nsliced %q\nwhole  %q", opt, s, w)
		}
	}
	const orders = `
context: orders
formatVersion: 1
---
entity: Order
context: orders
attributes:
  paid: { type: bool, controller: system }
  shipped: { type: bool, controller: system }
  express: { type: bool }
---
entity: Shipment
context: orders
attributes:
  tracked: { type: bool, controller: system }
  insured: { type: bool, controller: system }
---
operation: Order.ship
context: orders
default: allow
writes: [Order.shipped, Order.paid]
---
operation: Shipment.track
context: orders
default: allow
writes: [Shipment.tracked, Shipment.insured]
---
id: O1
kind: invariant
when: self.express
assert: self.shipped
subject: Order
---
id: O2
kind: invariant
assert: not self.shipped
subject: Order
---
id: S1
kind: invariant
assert: self.tracked
subject: Shipment
`
	cases := map[string]string{
		"independent parts": orders,
		// S2 contradicts S1 in every situation: the Shipment part cannot
		// hold, which rules out every value of the Order part too.
		"a part that cannot hold": orders + "---\nid: S2\nkind: invariant\nassert: not self.tracked\nsubject: Shipment\n",
	}
	for name, reg := range cases {
		s, w := checkSlicedAndWhole(t, writeRegistry(t, reg), lower.Options{})
		if !reflect.DeepEqual(s, w) {
			t.Errorf("%s:\nsliced %q\nwhole  %q", name, s, w)
		}
	}
}

// TestSplit: the state unit splits by shared variables, and a unit with
// fixed constraints (a step) does not split.
func TestSplit(t *testing.T) {
	prog := lowered(t, resources)
	for _, u := range prog.Units {
		parts := split(u)
		if u.Op != nil && len(parts) != 1 {
			t.Errorf("%s split into %d parts", u.Name, len(parts))
		}
		vars, rules := 0, 0
		for _, p := range parts {
			vars += len(p.Vars)
			rules += len(p.Rules)
		}
		if vars != len(u.Vars) || rules != len(u.Rules) {
			t.Errorf("%s: parts hold %d vars and %d rules of %d and %d", u.Name, vars, rules, len(u.Vars), len(u.Rules))
		}
	}
}
