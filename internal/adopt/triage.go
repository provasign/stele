// Package adopt implements the steps of adopting an existing spec corpus that
// follow confirmation (docs/08 W5 steps 5 to 9): grouping and triage drafted
// from git history, the baseline with open conflicts, notes written back to
// the spec files, and sync.
package adopt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/translate"
)

// Owner decisions on a triage item.
const (
	Pending = "pending" // not yet decided; the baseline refuses it unless told to keep it open
	Accept  = "accept"  // the newer statement wins: retire Retire
	Reverse = "reverse" // the older statements win: retire RetireIfReversed
	Open    = "open"    // leave the conflict open (D-044)
	Dismiss = "dismiss" // not a conflict; any checker conflict among its rules still becomes open
)

// CheckerOnly marks an item the checker found and no review finding named.
const CheckerOnly = "checker-only"

// Item is one conflict for the owner to triage (docs/08 W5 step 6).
type Item struct {
	ID         string   `yaml:"id"`
	Group      string   `yaml:"group"`
	Finding    int      `yaml:"review_finding,omitempty"` // 1-based, in the review
	Verdict    string   `yaml:"verdict"`                  // confirmed, not-reproduced, unproved or checker-only
	Statements []string `yaml:"statements"`
	Situation  string   `yaml:"situation"`
	// Newer and Older order the item's statements by date: Newer is the one
	// introduced last. Empty when the dates do not order them.
	Newer string   `yaml:"newer,omitempty"`
	Older []string `yaml:"older,omitempty"`
	Basis string   `yaml:"basis"`
	// SupersededBy maps each losing statement to the one that supersedes it.
	// Where the checker found conflicts it is drafted per conflict, between
	// the statements of that conflict: in a finding of three statements the
	// middle one may beat the oldest and still hold where the newest is
	// silent. Otherwise every older statement loses to Newer.
	SupersededBy map[string]string `yaml:"superseded_by,omitempty"`
	// Retire is what accepting retires: the losing statements' rules in the
	// checker's conflicts. RetireIfReversed is the winning statements'. Both
	// are empty when the checker found no conflict: accepting then records
	// the supersession for write-back and retires no rule.
	Retire           []string `yaml:"retire,omitempty"`
	RetireIfReversed []string `yaml:"retire_if_reversed,omitempty"`
	// Exposes and ExposesIfReversed are the conflicts each direction would
	// leave that the check does not find now: retiring a rule widens the
	// situations checked (Preview).
	Exposes           []string `yaml:"exposes,omitempty"`
	ExposesIfReversed []string `yaml:"exposes_if_reversed,omitempty"`
	Decision          string   `yaml:"decision"`
	Note              string   `yaml:"note,omitempty"`
}

// Group is a set of items sharing a statement, which is likely their root cause.
type Group struct {
	Root  string // the shared statement; empty for an item alone
	Items []int  // indexes into the items, in input order
}

// Draft builds the triage list: one item per review candidate, then one per
// checker conflict or dead response no candidate covers. Items are grouped
// and ordered so the groups explaining the most conflicts come first.
func Draft(cands []score.Candidate, res *checker.Result, reg *ir.Registry, stmts []translate.Statement) []Item {
	byID := map[string]translate.Statement{}
	for _, s := range stmts {
		byID[s.ID] = s
	}
	stmtOf := func(rule string) string { return score.StatementFor(rule, stmts, reg) }
	sets := conflictSets(res)

	var items []Item
	covered := make([]bool, len(sets))
	for i, c := range cands {
		it := Item{Finding: i + 1, Verdict: c.Verdict, Statements: c.Statements, Situation: c.Situation, Decision: Pending}
		in := map[string]bool{}
		for _, s := range c.Statements {
			in[s] = true
		}
		var matched [][]string
		for k, cs := range sets {
			st := statementsOf(cs.rules, stmtOf, byID)
			within, covers := len(st) >= 2, true
			for _, s := range st {
				within = within && in[s]
			}
			for _, s := range c.Statements {
				covers = covers && contains(st, s)
			}
			if within || covers {
				covered[k] = true
				matched = append(matched, cs.rules)
			}
		}
		direct(&it, byID, reg, matched, stmtOf)
		items = append(items, it)
	}
	for k, cs := range sets {
		if covered[k] {
			continue
		}
		it := Item{Verdict: CheckerOnly, Statements: statementsOf(cs.rules, stmtOf, byID), Situation: cs.situation, Decision: Pending}
		direct(&it, byID, reg, [][]string{cs.rules}, stmtOf)
		items = append(items, it)
	}
	return order(items)
}

type conflictSet struct {
	rules     []string
	situation string
}

// conflictSets lists the checker's conflicts and dead responses as rule sets.
func conflictSets(res *checker.Result) []conflictSet {
	var out []conflictSet
	for _, c := range res.Conflicts {
		parts := make([]string, len(c.Counterexample))
		for i, b := range c.Counterexample {
			parts[i] = b.Name + "=" + b.Value
		}
		where := "the state"
		if c.Unit != "state" {
			where = c.Unit
		}
		out = append(out, conflictSet{rules: c.Rules, situation: fmt.Sprintf("checker: no response of %s when %s", where, strings.Join(parts, ", "))})
	}
	for _, d := range res.Dead {
		s := "checker: " + d.Response + " can never happen"
		if d.NeverAllowed {
			s = "checker: " + d.Unit + " never runs: its default is deny and no rule allows it"
		}
		out = append(out, conflictSet{rules: d.Rules, situation: s})
	}
	return out
}

func statementsOf(rules []string, stmtOf func(string) string, byID map[string]translate.Statement) []string {
	var out []string
	for _, r := range rules {
		if s := stmtOf(r); byID[s].ID != "" && !contains(out, s) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// newest returns the statement introduced strictly last among ids, or "" and
// why the dates do not order them (D-046: statements introduced the same
// day, or with no date, get no direction).
func newest(ids []string, byID map[string]translate.Statement) (string, []string, string) {
	type dated struct{ id, when string }
	var ds []dated
	for _, id := range ids {
		ds = append(ds, dated{id, byID[id].Introduced})
	}
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].when > ds[j].when })
	switch {
	case len(ds) < 2:
		return "", nil, "one statement: its own rules conflict, so there is nothing to order"
	case ds[0].when == "" || ds[len(ds)-1].when == "":
		var missing []string
		for _, d := range ds {
			if d.when == "" {
				missing = append(missing, d.id)
			}
		}
		return "", nil, "no git date for " + strings.Join(missing, ", ") + ", so the draft has no direction"
	case ds[0].when == ds[1].when:
		return "", nil, fmt.Sprintf("%s and %s were introduced at the same time (%s), in one commit, so the draft has no direction", ds[0].id, ds[1].id, ds[0].when)
	}
	var older, dates []string
	for _, d := range ds[1:] {
		older = append(older, d.id)
		dates = append(dates, fmt.Sprintf("%s (%s)", d.id, d.when))
	}
	return ds[0].id, older, fmt.Sprintf("%s was introduced %s, after %s", ds[0].id, ds[0].when, strings.Join(dates, ", "))
}

// direct drafts the item's direction from git history (D-046): in each of
// the checker's conflicts, the statement introduced last supersedes the
// others in it. With no checker conflict, the item's newest statement
// supersedes the rest.
func direct(it *Item, byID map[string]translate.Statement, reg *ir.Registry, sets [][]string, stmtOf func(string) string) {
	it.Newer, it.Older, it.Basis = newest(it.Statements, byID)
	for _, rules := range sets {
		win, lose, _ := newest(statementsOf(rules, stmtOf, byID), byID)
		if win == "" {
			continue // this conflict stays undirected
		}
		for _, r := range rules {
			x := reg.Rule(r)
			if x == nil { // an operation's effect or from: not a rule to retire
				continue
			}
			pin := ir.Pin{Rule: x.ID, Version: x.Version}.String()
			switch s := stmtOf(r); {
			case s == win:
				it.RetireIfReversed = appendNew(it.RetireIfReversed, pin)
			case contains(lose, s):
				it.Retire = appendNew(it.Retire, pin)
				if it.SupersededBy == nil {
					it.SupersededBy = map[string]string{}
				}
				if prev := it.SupersededBy[s]; prev == "" || byID[win].Introduced > byID[prev].Introduced {
					it.SupersededBy[s] = win
				}
			}
		}
	}
	if len(it.SupersededBy) == 0 && it.Newer != "" {
		it.SupersededBy = map[string]string{}
		for _, s := range it.Older {
			it.SupersededBy[s] = it.Newer
		}
	}
}

// order groups items greedily: the statement shared by the most ungrouped
// items roots the next group. Items sharing no statement stand alone.
// Groups come largest first, and items are renumbered in reading order.
func order(items []Item) []Item {
	var groups []Group
	done := make([]bool, len(items))
	for {
		count := map[string]int{}
		for i, it := range items {
			if !done[i] {
				for _, s := range it.Statements {
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
		for i, it := range items {
			if !done[i] && contains(it.Statements, root) {
				g.Items = append(g.Items, i)
				done[i] = true
			}
		}
		groups = append(groups, g)
	}
	for i := range items {
		if !done[i] {
			groups = append(groups, Group{Items: []int{i}})
		}
	}
	var out []Item
	for gi, g := range groups {
		for _, i := range g.Items {
			it := items[i]
			it.Group = fmt.Sprintf("G-%d", gi+1)
			if g.Root != "" {
				it.Group += " (shares " + g.Root + ")"
			}
			it.ID = fmt.Sprintf("T-%d", len(out)+1)
			out = append(out, it)
		}
	}
	return out
}

// Carry copies the owner's decisions and notes from a previous triage onto
// the items found again, matched by statements and situation, and returns
// how many it kept. A drafted direction or retire list the owner edited is
// kept too, since the owner may have changed it before accepting.
func Carry(prev, items []Item) int {
	key := func(it Item) string {
		ss := append([]string{}, it.Statements...)
		sort.Strings(ss)
		return strings.Join(ss, "\x00") + "\x01" + it.Situation
	}
	old := map[string]Item{}
	for _, it := range prev {
		old[key(it)] = it
	}
	n := 0
	for i := range items {
		p, ok := old[key(items[i])]
		if !ok || (p.Decision == Pending && p.Note == "") {
			continue
		}
		items[i].Decision, items[i].Note = p.Decision, p.Note
		items[i].Newer, items[i].Older, items[i].SupersededBy = p.Newer, p.Older, p.SupersededBy
		items[i].Retire, items[i].RetireIfReversed = p.Retire, p.RetireIfReversed
		n++
	}
	return n
}

// Pairs lists who supersedes whom: "A supersedes B" for an accepted item,
// "B supersedes A" for a reversed one, sorted.
func (it *Item) Pairs() [][2]string {
	var out [][2]string
	for loser, winner := range it.SupersededBy {
		if it.Decision == Reverse {
			out = append(out, [2]string{loser, winner})
		} else {
			out = append(out, [2]string{winner, loser})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][1]+out[i][0] < out[j][1]+out[j][0] })
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func appendNew(xs []string, x string) []string {
	if contains(xs, x) {
		return xs
	}
	return append(xs, x)
}
