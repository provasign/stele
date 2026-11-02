package score

import (
	"strings"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// MeetingResult counts translated statements whose rules share a symbol with
// a statement from another spec: only those can contradict across specs.
type MeetingResult struct {
	Translated int `json:"translated"`
	Meeting    int `json:"meeting"`
}

// Meeting measures how well a vocabulary makes specs meet. A symbol is an
// attribute a rule reads or an operation it constrains; the spec is the
// statement ID's first segment (003/FR-025 is spec 003).
func Meeting(reg *ir.Registry, stmts []translate.Statement) MeetingResult {
	idx := index(stmts)
	uses := map[string]map[string]bool{} // statement -> symbols
	for _, x := range reg.Rules {
		if !x.Live() {
			continue
		}
		sid := statementFor(x.ID, idx, reg)
		if uses[sid] == nil {
			uses[sid] = map[string]bool{}
		}
		if op := x.Op(); op != "" {
			uses[sid][op] = true
		}
		subject, _ := reg.SubjectOf(x)
		for _, e := range []ir.Expr{x.When, x.Assert} {
			if e == nil {
				continue
			}
			for _, a := range reg.Reads(e, subject) {
				uses[sid][a.QName()] = true
			}
		}
	}
	specs := map[string]map[string]bool{} // symbol -> specs using it
	for sid, syms := range uses {
		spec, _, _ := strings.Cut(sid, "/")
		for s := range syms {
			if specs[s] == nil {
				specs[s] = map[string]bool{}
			}
			specs[s][spec] = true
		}
	}
	var r MeetingResult
	for _, syms := range uses {
		r.Translated++
		for s := range syms {
			if len(specs[s]) > 1 {
				r.Meeting++
				break
			}
		}
	}
	return r
}
