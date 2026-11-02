package translate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/equiv"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
)

// Statement statuses after clustering.
const (
	Agreed    = "agreed"    // every sample gave the same meaning
	Ambiguous = "ambiguous" // samples gave different meanings; the author must choose
	Answered  = "answered"  // was ambiguous; the author chose a reading
	Invalid   = "invalid"   // no sample gave a rule that compiles
)

// Reading is one distinct meaning the samples gave a statement.
type Reading struct {
	Samples []string `json:"samples"`         // which translations gave it
	Rules   []string `json:"rules,omitempty"` // the rules, as text
	Outcome string   `json:"outcome"`         // rule, informal or needs_vocabulary
	Missing []string `json:"missing_terms,omitempty"`
	// Candidates are the representative sample's candidates, so an answer to
	// the question can be applied without translating again.
	Candidates []Candidate `json:"candidates"`
}

// Question asks the author to choose between the two most common readings.
type Question struct {
	// Situation is a state where readings 1 and 2 differ; Says tells what
	// each reading makes of it. Empty when one reading is not a rule.
	Situation []checker.Binding `json:"situation,omitempty"`
	Says      [2]string         `json:"says"`
}

// StatementClusters is the clustering result for one statement.
type StatementClusters struct {
	StatementID string    `json:"statement_id"`
	Status      string    `json:"status"`
	Readings    []Reading `json:"readings"` // most samples first; the first one is kept
	Question    *Question `json:"question,omitempty"`
}

// meaning is one sample's translation of one statement, ready to compare.
type meaning struct {
	sample   int
	cands    []Candidate
	key      string // for non-rule outcomes: the outcomes; "" for rules
	formulas []*lower.Formula
	dts      []string
	missing  []string
}

func cluster(ctx context.Context, rep *Report, stmts []Statement, samples [][]Candidate, labels []string, opt Options) error {
	for _, s := range stmts {
		var ms []*meaning
		for k, cands := range samples {
			var mine []Candidate
			for _, c := range cands {
				if c.StatementID == s.ID && c.Error == "" {
					mine = append(mine, c)
				}
			}
			if len(mine) == 0 {
				continue
			}
			m, err := meaningOf(k, mine, opt.VocabFiles)
			if err != nil {
				return fmt.Errorf("statement %s, %s: %w", s.ID, labels[k], err)
			}
			ms = append(ms, m)
		}
		sc := StatementClusters{StatementID: s.ID}
		if len(ms) == 0 {
			sc.Status = Invalid
			rep.Clusters = append(rep.Clusters, sc)
			for _, cands := range samples { // keep the invalid candidates on record
				for _, c := range cands {
					if c.StatementID == s.ID {
						rep.Candidates = append(rep.Candidates, c)
					}
				}
				break
			}
			continue
		}
		var groups [][]*meaning
		for _, m := range ms {
			placed := false
			for gi, g := range groups {
				same, err := sameMeaning(ctx, opt, g[0], m)
				if err != nil {
					return fmt.Errorf("statement %s: %w", s.ID, err)
				}
				if same {
					groups[gi] = append(g, m)
					placed = true
					break
				}
			}
			if !placed {
				groups = append(groups, []*meaning{m})
			}
		}
		sort.SliceStable(groups, func(i, j int) bool { return len(groups[i]) > len(groups[j]) })
		for _, g := range groups {
			r := Reading{Outcome: "rule"}
			for _, m := range g {
				r.Samples = append(r.Samples, labels[m.sample])
			}
			rep0 := g[0]
			r.Candidates = rep0.cands
			if rep0.key != "" {
				r.Outcome, r.Missing = rep0.key, rep0.missing
			}
			for _, c := range rep0.cands {
				if c.Outcome == "rule" {
					r.Rules = append(r.Rules, candText(c))
				}
			}
			sc.Readings = append(sc.Readings, r)
		}
		chosen := groups[0][0].cands
		if len(groups) == 1 {
			sc.Status = Agreed
		} else {
			sc.Status = Ambiguous
			q := &Question{}
			a, b := groups[0][0], groups[1][0]
			if a.key == "" && b.key == "" {
				res, err := equiv.Compare(ctx, opt.Solver, append(append([]string{}, a.dts...), b.dts...), a.formulas, b.formulas)
				if err != nil {
					return fmt.Errorf("statement %s: %w", s.ID, err)
				}
				q.Situation = res.Witness
				q.Says = [2]string{verdict(res.AHolds), verdict(res.BHolds)}
			} else {
				q.Says = [2]string{describeOutcome(a), describeOutcome(b)}
			}
			sc.Question = q
			for i := range chosen {
				chosen[i].Draft = true
			}
		}
		rep.Candidates = append(rep.Candidates, chosen...)
		rep.Clusters = append(rep.Clusters, sc)
	}
	return nil
}

func verdict(holds bool) string {
	if holds {
		return "allows this state"
	}
	return "forbids this state"
}

func describeOutcome(m *meaning) string {
	if m.key == "" {
		return "a checkable rule"
	}
	if len(m.missing) > 0 {
		return m.key + " (missing: " + strings.Join(m.missing, ", ") + ")"
	}
	return m.key
}

// sameMeaning reports whether two readings mean the same: the same non-rule
// outcome, or rules the solver proves equivalent.
func sameMeaning(ctx context.Context, opt Options, a, b *meaning) (bool, error) {
	if a.key != "" || b.key != "" {
		return a.key == b.key, nil
	}
	res, err := equiv.Compare(ctx, opt.Solver, append(append([]string{}, a.dts...), b.dts...), a.formulas, b.formulas)
	if err != nil {
		return false, err
	}
	return res.Equivalent, nil
}

// meaningOf lowers one sample's rules for one statement, alone with the vocabulary.
func meaningOf(sample int, cands []Candidate, vocabFiles []string) (*meaning, error) {
	m := &meaning{sample: sample, cands: cands}
	var rules []Candidate
	outcomes := map[string]bool{}
	for _, c := range cands {
		if c.Outcome == "rule" {
			rules = append(rules, c)
		} else {
			outcomes[c.Outcome] = true
			m.missing = append(m.missing, c.MissingTerms...)
		}
	}
	if len(rules) == 0 {
		keys := make([]string, 0, len(outcomes))
		for k := range outcomes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m.key = strings.Join(keys, "+")
		return m, nil
	}
	docs := make([]map[string]any, len(rules))
	ids := map[string]bool{}
	for i := range rules {
		docs[i] = ruleDoc(&rules[i], nil)
		ids[rules[i].RuleID] = true
	}
	src, err := marshalDocs(docs)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "stele-cluster-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "candidate.yaml")
	if err := os.WriteFile(f, src, 0o644); err != nil {
		return nil, err
	}
	reg, err := ir.LoadFiles(append(append([]string{}, vocabFiles...), f)...)
	if err != nil {
		return nil, err // each rule compiled alone; together they can still clash, e.g. two defaults
	}
	prog, err := lower.Lower(reg, lower.Options{IgnoreDecisions: true, AssumeDefaults: true})
	if err != nil {
		return nil, err
	}
	for _, u := range prog.Units {
		for _, group := range [][]*lower.Formula{u.Rules, u.Assumptions} {
			for _, fm := range group {
				if fm.Home && ids[fm.Rule.ID] {
					m.formulas = append(m.formulas, fm)
				}
			}
		}
	}
	m.dts = prog.Datatypes
	return m, nil
}

// candText renders a candidate rule as one line.
func candText(c Candidate) string {
	var b strings.Builder
	b.WriteString(c.Kind)
	if c.Kind == ir.Permission {
		b.WriteString(" " + c.Effect)
	}
	if c.Operation != "" {
		b.WriteString(" " + c.Operation)
	}
	if c.On != "" {
		b.WriteString(" on " + c.On)
	}
	if c.Subject != "" {
		b.WriteString(" (subject " + c.Subject + ")")
	}
	if c.When != "" {
		b.WriteString(" when " + c.When)
	}
	if c.Assert != "" {
		b.WriteString(" assert " + c.Assert)
	}
	return b.String()
}

// questionsMarkdown renders the open questions for the author.
func questionsMarkdown(rep *Report, stmts []Statement) string {
	prose := map[string]Statement{}
	for _, s := range stmts {
		prose[s.ID] = s
	}
	var b strings.Builder
	n := 0
	for _, sc := range rep.Clusters {
		if sc.Status != Ambiguous {
			continue
		}
		n++
		s := prose[sc.StatementID]
		fmt.Fprintf(&b, "## %d. %s\n\n> %s\n\nSource: %s\n\n", n, sc.StatementID, s.Prose, s.Source)
		for i, r := range sc.Readings {
			fmt.Fprintf(&b, "**Reading %d** (%d of %d samples: %s)", i+1, len(r.Samples), len(rep.Samples), strings.Join(r.Samples, ", "))
			if r.Outcome != "rule" {
				fmt.Fprintf(&b, ": %s", r.Outcome)
				if len(r.Missing) > 0 {
					fmt.Fprintf(&b, ", missing %s", strings.Join(r.Missing, ", "))
				}
				b.WriteString("\n\n")
				continue
			}
			b.WriteString("\n\n")
			for _, rule := range r.Rules {
				fmt.Fprintf(&b, "- `%s`\n", rule)
			}
			b.WriteString("\n")
		}
		if q := sc.Question; q != nil && len(q.Situation) > 0 {
			fmt.Fprintf(&b, "They differ when %s. Reading 1 %s; reading 2 %s.\n\n", checker.Plain(checker.Situation(nil, q.Situation)), q.Says[0], q.Says[1])
		} else if q != nil {
			fmt.Fprintf(&b, "Reading 1 is %s; reading 2 is %s.\n\n", q.Says[0], q.Says[1])
		}
		b.WriteString("Which reading is meant? Until answered, reading 1 is written as a draft and not checked.\n\n")
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("# Questions from stele translate\n\n%d statement(s) were read differently by different samples.\n\n", n) + b.String()
}

// LoadRun reads the report of a translation run directory.
func LoadRun(dir string) (*Report, error) {
	src, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal(src, &rep); err != nil {
		return nil, fmt.Errorf("%s/report.json: %w", dir, err)
	}
	return &rep, nil
}

// Answer applies the author's choice of reading (1-based) for an ambiguous
// statement: that reading's rules replace the draft and become active.
func Answer(rep *Report, statementID string, reading int) error {
	for i := range rep.Clusters {
		sc := &rep.Clusters[i]
		if sc.StatementID != statementID {
			continue
		}
		if sc.Status != Ambiguous && sc.Status != Answered {
			return fmt.Errorf("statement %s is %s, not ambiguous", statementID, sc.Status)
		}
		if reading < 1 || reading > len(sc.Readings) {
			return fmt.Errorf("statement %s has readings 1..%d", statementID, len(sc.Readings))
		}
		var kept []Candidate
		for _, c := range rep.Candidates {
			if c.StatementID != statementID {
				kept = append(kept, c)
			}
		}
		for _, c := range sc.Readings[reading-1].Candidates {
			c.Draft = false
			kept = append(kept, c)
		}
		rep.Candidates = kept
		sc.Status = Answered
		return nil
	}
	return fmt.Errorf("no statement %s in this run", statementID)
}

// RunVocabulary lists the vocabulary files WriteRegistry copied into dir.
func RunVocabulary(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".yaml") && n != "rules.yaml" {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out, nil
}
