// Package lint finds rules whose shape shows a translation error, whatever
// the corpus: they would be reported as conflicts in the specs though the
// statements do not disagree. Findings are for the translator or the
// reviewer to fix; the checker still checks the rules as written.
package lint

import (
	"fmt"
	"sort"

	"github.com/provasign/stele/internal/ir"
)

// Finding is one rule with a suspect shape.
type Finding struct {
	Rule    string `json:"rule"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Kinds of finding.
const (
	// InputsOnly: part of an invariant on an operation reads only what the
	// operation does not write, so running the operation cannot make it hold;
	// whenever the inputs disagree, the rule alone leaves no response.
	InputsOnly = "asserts-inputs"
	// SelfConflict: a conflict among rules of one statement.
	SelfConflict = "self-conflict"
)

// Rules lints every rule of reg.
func Rules(reg *ir.Registry) []Finding {
	var out []Finding
	for _, x := range reg.Rules {
		if !x.Live() || x.Kind != ir.Invariant || x.On == "" || x.Assert == nil {
			continue
		}
		op := reg.Operations[x.On]
		if op == nil { // a pattern such as "*"; nothing single to compare with
			continue
		}
		subject, err := reg.SubjectOf(x)
		if err != nil {
			continue
		}
		written := map[*ir.Attr]bool{}
		for _, a := range op.Writes {
			written[a] = true
		}
		for _, e := range op.Effect {
			written[e.Attr] = true
		}
		if h := op.History; h != nil {
			written[h.Ran], written[h.Runs], written[h.Last] = true, true, true
		}
		for _, part := range conjuncts(x.Assert) {
			reads := reg.Reads(part, subject)
			if len(reads) == 0 {
				continue
			}
			inputs := true
			for _, a := range reads {
				inputs = inputs && !written[a]
			}
			if inputs {
				out = append(out, Finding{Rule: x.ID, Kind: InputsOnly, Message: fmt.Sprintf(
					"%s does not write what %q reads, so running it cannot make that hold; state it as a precondition, in \"when\", or as a write of %s", op.Name, part.String(), op.Name)})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}

func conjuncts(e ir.Expr) []ir.Expr {
	if b, ok := e.(*ir.Binary); ok && b.Op == "and" {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	return []ir.Expr{e}
}

// SelfConflicts returns the conflicts (given as rule IDs) whose rules all
// come from one statement: a statement rarely contradicts itself, its
// translation often does.
func SelfConflicts(reg *ir.Registry, conflicts [][]string) []int {
	var out []int
	for i, ids := range conflicts {
		stmt, one := "", true
		for _, id := range ids {
			r := reg.Rule(id)
			if r == nil {
				one = false
				break
			}
			if stmt == "" {
				stmt = r.Statement()
			}
			one = one && r.Statement() == stmt
		}
		if one && len(ids) > 0 {
			out = append(out, i)
		}
	}
	return out
}
