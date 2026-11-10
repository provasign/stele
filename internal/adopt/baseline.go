package adopt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// Files the baseline writes inside the registry; each run replaces them.
const (
	DecisionsFile = "decisions/adoption.yaml"
	ConflictsFile = "conflicts/adoption.yaml"
)

// BaselineResult is what a baseline wrote.
type BaselineResult struct {
	Decisions int
	Retired   int
	Open      int
	// Recorded counts accepted or reversed items that retire no rule: the
	// checker found no conflict for them, so they only drive write-back.
	Recorded int
	// Remaining counts, per accepted or reversed item, the open conflicts
	// still among its statements. Retiring a rule can expose conflicts it
	// hid: an invariant on the state before a step limits the situations
	// checked, so retiring it adds situations. The owner may retire more.
	Remaining map[string]int
	Verdict   string // of the check with decisions and open conflicts applied
}

// Options for a baseline.
type Options struct {
	By          []string // owners acknowledging the open conflicts (D-044)
	Since       string   // date recorded on each open conflict
	KeepPending bool     // treat pending items as open instead of refusing
}

// Baseline writes a decision for each accepted or reversed item that retires
// rules, checks the registry with them, and records every conflict and dead
// response left as an open conflict pinned to its rules' versions (docs/08
// W5 step 7). The registry then passes the gate, and any later conflict
// blocks.
func Baseline(ctx context.Context, z *solver.Z3, dir string, items []Item, stmts []translate.Statement, opt Options) (*BaselineResult, error) {
	var pending []string
	for _, it := range items {
		switch it.Decision {
		case Pending, "":
			pending = append(pending, it.ID)
		case Accept, Reverse, Open, Dismiss:
		default:
			return nil, fmt.Errorf("%s: decision %q (want accept, reverse, open, dismiss or pending)", it.ID, it.Decision)
		}
	}
	if len(pending) > 0 && !opt.KeepPending {
		return nil, fmt.Errorf("%d item(s) still pending (%s); decide them, or keep them open with --keep-pending", len(pending), summary(pending))
	}
	if len(opt.By) == 0 {
		return nil, fmt.Errorf("an open conflict needs the owners who acknowledge it (--by)")
	}
	for _, f := range []string{DecisionsFile, ConflictsFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		return nil, err
	}

	res := &BaselineResult{}
	var decisions []any
	retired := map[string]bool{}
	for _, it := range items {
		var retire []string
		switch it.Decision {
		case Accept:
			retire = it.Retire
		case Reverse:
			retire = it.RetireIfReversed
		default:
			continue
		}
		var pairs []string
		for _, p := range it.Pairs() {
			pairs = append(pairs, p[0]+" supersedes "+p[1])
		}
		why := strings.Join(pairs, "; ")
		if len(retire) == 0 {
			res.Recorded++
			continue
		}
		resolves := append(append([]string{}, it.Retire...), it.RetireIfReversed...)
		sort.Strings(resolves)
		rationale := fmt.Sprintf("Adoption triage %s (%s): %s where they conflict. %s.", it.ID, it.Verdict, why, it.Basis)
		if it.Note != "" {
			rationale += " " + it.Note
		}
		decisions = append(decisions, decisionDoc{ID: "D-" + it.ID, Resolves: resolves, Retire: retire, Rationale: rationale})
		for _, p := range retire {
			retired[p] = true
		}
	}
	res.Decisions, res.Retired = len(decisions), len(retired)
	if err := writeDocs(filepath.Join(dir, DecisionsFile), decisions); err != nil {
		return nil, err
	}

	if reg, err = ir.LoadDir(dir); err != nil {
		return nil, fmt.Errorf("with the drafted decisions: %w", err)
	}
	check, err := run(ctx, z, reg)
	if err != nil {
		return nil, err
	}
	if len(check.Undecided) > 0 {
		return nil, fmt.Errorf("cannot baseline an incomplete check (solver or conflict limit): %s", strings.Join(check.Undecided, ", "))
	}
	var open []any
	pin := func(rules []string) []string {
		var out []string
		for _, id := range rules {
			if x := reg.Rule(id); x != nil {
				out = append(out, ir.Pin{Rule: x.ID, Version: x.Version}.String())
			} else {
				out = append(out, id)
			}
		}
		return out
	}
	res.Remaining = map[string]int{}
	add := func(rules []string, response, situation string) {
		text, from := situationFor(rules, situation, items, stmts, reg)
		if from != nil && (from.Decision == Accept || from.Decision == Reverse) {
			res.Remaining[from.ID]++
		}
		open = append(open, openDoc{
			ID: fmt.Sprintf("OC-%d", len(open)+1), Rules: pin(rules), Response: response,
			Situation: text, AcknowledgedBy: opt.By, Since: opt.Since, Status: "open",
		})
	}
	for _, c := range check.Conflicts {
		parts := make([]string, len(c.Counterexample))
		for i, b := range c.Counterexample {
			parts[i] = b.Name + "=" + b.Value
		}
		add(c.Rules, "", "no response of "+c.Unit+" when "+strings.Join(parts, ", "))
	}
	for _, d := range check.Dead {
		add(d.Rules, d.Response, d.Response+" can never happen")
	}
	res.Open = len(open)
	if err := writeDocs(filepath.Join(dir, ConflictsFile), open); err != nil {
		return nil, err
	}

	// The baseline must pass its own gate.
	if reg, err = ir.LoadDir(dir); err != nil {
		return nil, fmt.Errorf("with the open conflicts: %w", err)
	}
	verified, err := gate.Registries(ctx, z, nil, reg)
	if err != nil {
		return nil, err
	}
	if verified.Check == nil {
		return nil, fmt.Errorf("baseline gate: %s", strings.Join(verified.Errors, "; "))
	}
	res.Verdict = verified.Check.Verdict
	return res, nil
}

func run(ctx context.Context, z *solver.Z3, reg *ir.Registry) (*checker.Result, error) {
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		return nil, err
	}
	return checker.Check(ctx, z, prog)
}

// situationFor names the item an open conflict came from: the smallest item
// whose statements include every statement of the conflict.
func situationFor(rules []string, fallback string, items []Item, stmts []translate.Statement, reg *ir.Registry) (string, *Item) {
	byID := map[string]translate.Statement{}
	for _, s := range stmts {
		byID[s.ID] = s
	}
	st := statementsOf(rules, func(r string) string { return score.StatementFor(r, stmts, reg) }, byID)
	var best *Item
	for i := range items {
		it := &items[i]
		ok := len(st) > 0
		for _, s := range st {
			ok = ok && contains(it.Statements, s)
		}
		if ok && (best == nil || len(it.Statements) < len(best.Statements)) {
			best = it
		}
	}
	if best == nil {
		return "checker: " + fallback, nil
	}
	return fmt.Sprintf("%s (%s, decision %s): %s", best.ID, best.Verdict, best.Decision, best.Situation), best
}

type decisionDoc struct {
	ID        string   `yaml:"id"`
	Resolves  []string `yaml:"resolves"`
	Retire    []string `yaml:"retire"`
	Rationale string   `yaml:"rationale"`
}

// LeftOpen is a conflict the owner chose to leave open at adoption.
type LeftOpen struct {
	Rules     []string
	Response  string // a step that can never run, rather than a conflict
	Situation string
	By        string
}

// WriteOpen records the conflicts as open in the registry in dir (D-044),
// replacing the adoption's earlier ones: each pins its rules at their
// versions, and the gate tolerates exactly those.
func WriteOpen(dir string, reg *ir.Registry, open []LeftOpen, since, name string) error {
	sort.Slice(open, func(i, j int) bool {
		a, b := strings.Join(open[i].Rules, ","), strings.Join(open[j].Rules, ",")
		return a < b || a == b && open[i].Response < open[j].Response
	})
	var docs []any
	for k, o := range open {
		var pins []string
		for _, id := range o.Rules {
			if x := reg.Rule(id); x != nil {
				pins = append(pins, ir.Pin{Rule: x.ID, Version: x.Version}.String())
			} else {
				pins = append(pins, id)
			}
		}
		id := fmt.Sprintf("OC-%d", k+1)
		if name != "" {
			id = fmt.Sprintf("OC-%s-%d", name, k+1)
		}
		docs = append(docs, openDoc{ID: id, Rules: pins, Response: o.Response, Situation: o.Situation, AcknowledgedBy: []string{o.By}, Since: since, Status: "open"})
	}
	path := filepath.Join(dir, ConflictsFile)
	if name != "" {
		// A part of the specs adopted on its own keeps its open conflicts
		// in its own file, beside those of the parts adopted before.
		path = filepath.Join(dir, strings.TrimSuffix(ConflictsFile, ".yaml")+"-"+name+".yaml")
	}
	if len(docs) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeDocs(path, docs)
}

type openDoc struct {
	ID             string   `yaml:"id"`
	Rules          []string `yaml:"rules,omitempty"`
	Response       string   `yaml:"response,omitempty"`
	Situation      string   `yaml:"situation"`
	AcknowledgedBy []string `yaml:"acknowledgedBy"`
	Since          string   `yaml:"since"`
	Status         string   `yaml:"status"`
}

func writeDocs(path string, docs []any) error {
	if len(docs) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# Written by stele baseline (docs/08 W5); rerunning it replaces this file.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func summary(ids []string) string {
	if len(ids) > 5 {
		return strings.Join(ids[:5], ", ") + fmt.Sprintf(" and %d more", len(ids)-5)
	}
	return strings.Join(ids, ", ")
}
