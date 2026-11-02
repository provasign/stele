// Package score compares a translated registry with a hand-written reference,
// with the solver and never with an LLM judge (stele-research docs/design-archive/12-benchmarking.md):
//
//   - per statement, are the translated rules logically equivalent to the
//     reference rules over the declared ranges?
//   - over the registry, does the checker find the same minimal conflicts?
package score

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/equiv"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// Statement verdicts.
const (
	Equivalent   = "equivalent"
	Different    = "different"
	Untranslated = "untranslated" // only informal or needs-vocabulary output, or none
	Unknown      = "unknown"
)

// StatementResult scores one statement.
type StatementResult struct {
	ID        string            `json:"id"`
	Verdict   string            `json:"verdict"`
	Reference []string          `json:"reference"`
	Candidate []string          `json:"candidate"`
	Witness   []checker.Binding `json:"witness,omitempty"` // a state where the two disagree
	RefIR     []string          `json:"reference_ir"`
	CandIR    []string          `json:"candidate_ir"`
}

// Result is the full comparison.
type Result struct {
	Statements []StatementResult `json:"statements"`
	Equivalent int               `json:"equivalent"`
	Total      int               `json:"total"`
	Found      []string          `json:"conflicts_found"`  // reference conflicts the candidate also has
	Missed     []string          `json:"conflicts_missed"` // reference conflicts the candidate lacks
	Extra      []string          `json:"conflicts_extra"`  // candidate conflicts the reference lacks
}

// Score compares candDir against refDir for the given statements. Both are
// lowered without decisions: the experiment is about the rules as written.
func Score(ctx context.Context, z *solver.Z3, refDir, candDir string, stmts []translate.Statement) (*Result, error) {
	refReg, err := ir.LoadDir(refDir)
	if err != nil {
		return nil, fmt.Errorf("reference: %w", err)
	}
	candReg, err := ir.LoadDir(candDir)
	if err != nil {
		return nil, fmt.Errorf("candidate: %w", err)
	}
	refProg, err := lower.Lower(refReg, lower.Options{IgnoreDecisions: true, AssumeDefaults: true})
	if err != nil {
		return nil, fmt.Errorf("reference: %w", err)
	}
	candProg, err := lower.Lower(candReg, lower.Options{IgnoreDecisions: true, AssumeDefaults: true})
	if err != nil {
		return nil, fmt.Errorf("candidate: %w", err)
	}

	refStmt := map[string]string{} // reference rule ID -> statement ID
	for _, s := range stmts {
		for _, r := range s.Reference {
			refStmt[r] = s.ID
		}
	}
	res := &Result{Total: len(stmts)}
	for _, s := range stmts {
		sr := StatementResult{ID: s.ID, Reference: s.Reference}
		refF := formulas(refProg, func(id string) bool { return refStmt[id] == s.ID })
		candF := formulas(candProg, func(id string) bool { return translate.StatementOf(id) == s.ID })
		for _, f := range refF {
			sr.RefIR = append(sr.RefIR, describe(f.Rule))
		}
		for _, x := range candReg.Rules {
			if translate.StatementOf(x.ID) == s.ID {
				sr.Candidate = append(sr.Candidate, x.ID)
				sr.CandIR = append(sr.CandIR, describe(x))
			}
		}
		switch {
		case len(candF) == 0:
			sr.Verdict = Untranslated
		default:
			v, w, err := equivalent(ctx, z, refProg, candProg, refF, candF)
			if err != nil {
				return nil, fmt.Errorf("statement %s: %w", s.ID, err)
			}
			sr.Verdict, sr.Witness = v, w
		}
		if sr.Verdict == Equivalent {
			res.Equivalent++
		}
		res.Statements = append(res.Statements, sr)
	}

	refConf, err := checker.Check(ctx, z, refProg)
	if err != nil {
		return nil, err
	}
	candConf, err := checker.Check(ctx, z, candProg)
	if err != nil {
		return nil, err
	}
	refSet := coreSet(refConf, func(id string) string {
		if s, ok := refStmt[id]; ok {
			return s
		}
		return id // world facts keep their IDs in both registries
	})
	candSet := coreSet(candConf, translate.StatementOf)
	for k := range refSet {
		if candSet[k] {
			res.Found = append(res.Found, k)
		} else {
			res.Missed = append(res.Missed, k)
		}
	}
	for k := range candSet {
		if !refSet[k] {
			res.Extra = append(res.Extra, k)
		}
	}
	sort.Strings(res.Found)
	sort.Strings(res.Missed)
	sort.Strings(res.Extra)
	return res, nil
}

func describe(x *ir.Rule) string {
	s := x.ID + ": " + x.Kind
	if x.Effect != "" {
		s += " " + x.Effect
	}
	if x.Operation != "" {
		s += " " + x.Operation
	}
	if x.Subject != "" {
		s += " subject " + x.Subject
	}
	if x.When != nil {
		s += " when " + x.When.String()
	}
	if x.Assert != nil {
		s += " assert " + x.Assert.String()
	}
	return s
}

// formulas returns, for every rule whose ID matches, its formulas in its own
// unit: the state unit for state invariants and defaults, its operation's
// unit otherwise. Both registries share a vocabulary, so the same symbol has
// the same name in each.
func formulas(p *lower.Program, match func(string) bool) []*lower.Formula {
	var out []*lower.Formula
	for _, u := range p.Units {
		for _, group := range [][]*lower.Formula{u.Rules, u.Assumptions} {
			for _, f := range group {
				if f.Home && match(f.Rule.ID) {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// equivalent compares the reference and candidate readings of one statement.
func equivalent(ctx context.Context, z *solver.Z3, refProg, candProg *lower.Program, ref, cand []*lower.Formula) (string, []checker.Binding, error) {
	r, err := equiv.Compare(ctx, z, append(append([]string{}, refProg.Datatypes...), candProg.Datatypes...), ref, cand)
	switch {
	case err != nil:
		return "", nil, err
	case r.Equivalent:
		return Equivalent, nil, nil
	case r.Unknown:
		return Unknown, nil, nil
	}
	return Different, r.Witness, nil
}

// coreSet names each conflict and dead response by the sorted statement IDs
// of its rules: a contradiction is the same finding whichever form it takes.
func coreSet(r *checker.Result, toStmt func(string) string) map[string]bool {
	out := map[string]bool{}
	cores := make([][]string, 0, len(r.Conflicts)+len(r.Dead))
	for _, c := range r.Conflicts {
		cores = append(cores, c.Rules)
	}
	for _, d := range r.Dead {
		cores = append(cores, d.Rules)
	}
	for _, rules := range cores {
		ids := map[string]bool{}
		for _, id := range rules {
			ids[toStmt(id)] = true
		}
		keys := make([]string, 0, len(ids))
		for k := range ids {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out[strings.Join(keys, " + ")] = true
	}
	return out
}
