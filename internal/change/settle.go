package change

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/translate"
)

// At an adoption the owner is asked about a contradiction only when a
// reading of the texts backs it. A coding agent adopting 179 requirements
// brought its owner 22 contradictions after 42 minutes; the review of the
// texts backed one of them, and the rest were clashes between Stele's own
// rules that no reader of the specs sees: a "SHALL create" against a check
// before it, a step taken to run on an existing instance, a reading turned
// round (2026-11-10). On two labelled sets every contradiction the rules
// proved was among the review's findings. So the review says what deserves
// the owner, the solver proves it, and what only the rules show Stele
// settles itself, in the same run: by precedence where one requirement is a
// check or an error case, else recorded as open. The approval says how
// many, and the report lists each.

// Settler is who records the answers Stele gives itself.
const Settler = "stele"

// backed reports whether a review finding names two of the question's
// requirements together.
func backed(q Question, findings [][]string) bool {
	var ids []string
	for _, r := range q.Requirements {
		ids = append(ids, r.ID)
	}
	for _, f := range findings {
		n := 0
		for _, id := range f {
			if slices.Contains(ids, id) {
				n++
			}
		}
		if n >= 2 {
			return true
		}
	}
	return false
}

// settle answers, for Stele, the questions of an adoption that are about
// its own rules and not about the specs, and returns how many it answered:
// the check then runs again with them. It does nothing until the texts
// have been reviewed.
func settle(opt Options, res *Result, as []Answer) int {
	if opt.Adopt == nil || opt.NoReview {
		return 0
	}
	st := loadCross(opt.Out)
	if st.Key == "" {
		return 0 // no review to go by: every question is asked, as before
	}
	var findings [][]string
	for _, f := range st.Findings {
		findings = append(findings, f.StatementIDs)
	}
	n := 0
	save := func(a Answer) {
		// The same answer given before and the question still here: it is
		// asked, not answered round after round.
		if slices.ContainsFunc(as, func(x Answer) bool { return x.Question == a.Question && x.By == Settler && x.Choice == a.Choice }) {
			return
		}
		a.By = Settler
		if err := SaveAnswer(opt.Out, a); err == nil {
			n++
		}
	}
	hashes := func(q Question, only []string) map[string]string {
		out := map[string]string{}
		for _, r := range q.Requirements {
			if only == nil || slices.Contains(only, r.ID) {
				out[r.ID] = translate.TextHash(r.Text)
			}
		}
		return out
	}
	for _, q := range res.Questions {
		switch q.Kind {
		case "creates":
			// Whether a step makes a new instance only decides whether a
			// clash of the rules is asked; unbacked, it is not asked.
			c := q.Recommended
			if c == "" {
				c = "existing"
			}
			save(Answer{Question: q.ID, Kind: q.Kind, Choice: c, Term: q.Term, Existing: q.Existing, Statements: map[string]string{}})
		case "creator":
			save(Answer{Question: q.ID, Kind: q.Kind, Choice: "none", Existing: q.Existing, Statements: map[string]string{}})
		case "self_conflict":
			// Read again by the translation itself and still contradicting
			// itself: recorded without rules, as the owner is recommended.
			save(Answer{Question: q.ID, Kind: q.Kind, Choice: "unchecked", Statements: hashes(q, nil)})
		case "conflict":
			if len(q.Links) > 0 || backed(q, findings) {
				continue
			}
			k := slices.IndexFunc(q.Options, func(o Option) bool { return o.Choice == q.Recommended && len(o.Prevails) > 0 })
			switch {
			case k >= 0:
				o := q.Options[k]
				save(Answer{Question: q.ID, Kind: q.Kind, Choice: o.Choice, Statements: hashes(q, o.Prevails), Over: o.Over})
			case slices.ContainsFunc(q.Options, func(o Option) bool { return o.Choice == "later" }):
				save(Answer{Question: q.ID, Kind: q.Kind, Choice: "later", Statements: hashes(q, nil)})
			}
		}
	}
	return n
}

// settled counts what Stele settled itself in an adoption, for the
// approval: clashes recorded as open, clashes settled by precedence, and
// requirements recorded without rules.
func settled(as []Answer) (open, precedence, unchecked int, ids []string) {
	for _, a := range as {
		if a.By != Settler {
			continue
		}
		switch {
		case a.Kind == "conflict" && a.Choice == "later":
			open++
		case a.Kind == "conflict" && strings.HasPrefix(a.Choice, "prevails"):
			precedence++
		case a.Kind == "self_conflict" && a.Choice == "unchecked":
			unchecked++
		default:
			continue
		}
		for id := range a.Statements {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return open, precedence, unchecked, ids
}

// settledText says what Stele settled itself, for the approval question.
func settledText(open, precedence, unchecked int) string {
	var parts []string
	if open > 0 {
		parts = append(parts, fmt.Sprintf("%d recorded as open", open))
	}
	if precedence > 0 {
		parts = append(parts, fmt.Sprintf("%d settled by taking the check or the error case as coming first", precedence))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("Stele's rules clash in %d more place(s) that a reading of the specs' texts does not show as contradictions: %s. The report lists each, and `stele check` the open ones", open+precedence, strings.Join(parts, ", "))
}
