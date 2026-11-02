package score

import (
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/translate"
)

// Verdicts of docs/08 W5 step 4.
const (
	Confirmed     = "confirmed"      // the checker finds the contradiction, with a counterexample
	NotReproduced = "not-reproduced" // every statement became a rule and the rules hold together
	Unproved      = "unproved"       // some statement has no checked rule
)

// Candidate is one review finding after the check.
type Candidate struct {
	Statements []string `json:"statement_ids"`
	Situation  string   `json:"situation"`
	Verdict    string   `json:"verdict"`
	By         int      `json:"by_finding,omitempty"` // 1-based index into the checker findings
	// Unmodeled lists the candidate's statements with no active rule.
	Unmodeled []string `json:"unmodeled,omitempty"`
}

// Confirm checks each review finding against the checker's findings on a
// registry translated from the candidates' statements. A candidate is
// confirmed when a checker finding cites all of its statements, or when the
// checker finds a conflict among two or more of them alone; a conflict in a
// subset still means the set cannot all hold. A candidate with a statement
// that has no active rule (informal, missing vocabulary, an unanswered
// question) is unproved. Otherwise it is not reproduced: the rules hold
// together. That is not a refutation. On Project B most such candidates were
// real contradictions the vocabulary or translation missed, so the owner
// triages them like any other finding.
func Confirm(rv *review.Result, reg *ir.Registry, findings []Finding, stmts []translate.Statement) []Candidate {
	idx := index(stmts)
	modeled := map[string]bool{}
	for _, r := range reg.Rules {
		if r.Live() && r.Kind != ir.Informal && r.Kind != ir.Measured {
			modeled[statementFor(r.ID, idx, reg)] = true
		}
	}
	var out []Candidate
	for _, f := range rv.Findings {
		c := Candidate{Statements: f.StatementIDs, Situation: f.Situation}
		srcs := sourcesOf(f.StatementIDs, idx)
		inCandidate := map[string]bool{}
		for _, s := range srcs {
			inCandidate[s] = true
		}
		for i, cf := range findings {
			within := len(cf.Sources) >= 2
			for _, s := range cf.Sources {
				within = within && inCandidate[s]
			}
			if covers(cf.Sources, srcs) || within {
				c.Verdict, c.By = Confirmed, i+1
				break
			}
		}
		if c.Verdict == "" {
			for _, id := range f.StatementIDs {
				if !modeled[id] {
					c.Unmodeled = append(c.Unmodeled, id)
				}
			}
			c.Verdict = NotReproduced
			if len(c.Unmodeled) > 0 {
				c.Verdict = Unproved
			}
		}
		out = append(out, c)
	}
	return out
}

// Candidates returns the statements any review finding cites, in corpus order.
func Candidates(rv *review.Result, stmts []translate.Statement) []translate.Statement {
	cited := map[string]bool{}
	for _, f := range rv.Findings {
		for _, id := range f.StatementIDs {
			cited[id] = true
		}
	}
	var out []translate.Statement
	for _, s := range stmts {
		if cited[s.ID] {
			out = append(out, s)
		}
	}
	return out
}
