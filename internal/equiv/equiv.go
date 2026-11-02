// Package equiv decides whether two sets of lowered rules mean the same, with
// the solver. It backs translation scoring and translation clustering, both of
// which compare readings of one statement over a shared vocabulary.
package equiv

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// Result of comparing two readings.
type Result struct {
	Equivalent bool
	Unknown    bool
	// Witness is a state where the two differ (only when neither equivalent
	// nor unknown); AHolds and BHolds say which reading accepts that state.
	Witness        []checker.Binding
	AHolds, BHolds bool
}

func conj(fs []*lower.Formula) string {
	switch len(fs) {
	case 0:
		return "true"
	case 1:
		return fs[0].Term
	}
	terms := make([]string, len(fs))
	for i, f := range fs {
		terms[i] = f.Term
	}
	return "(and " + strings.Join(terms, " ") + ")"
}

// Compare asks whether the conjunctions of a and b can differ in any state
// within the declared ranges. Both must come from the same vocabulary, so a
// symbol has the same name in each; datatypes lists every enum declaration
// either side needs.
func Compare(ctx context.Context, z *solver.Z3, datatypes []string, a, b []*lower.Formula) (Result, error) {
	var sb strings.Builder
	sb.WriteString("(set-option :produce-models true)\n(set-logic ALL)\n")
	seen := map[string]bool{}
	for _, d := range datatypes {
		if !seen[d] {
			seen[d] = true
			sb.WriteString(d + "\n")
		}
	}
	vars := map[string]*lower.Var{}
	for _, f := range append(append([]*lower.Formula{}, a...), b...) {
		for _, v := range f.Vars {
			vars[v.Name] = v
		}
	}
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := vars[n]
		fmt.Fprintf(&sb, "(declare-const %s %s)\n", v.Name, v.Sort)
		if v.Range != "" {
			fmt.Fprintf(&sb, "(assert %s)\n", v.Range)
		}
	}
	ta, tb := conj(a), conj(b)
	fmt.Fprintf(&sb, "(assert (not (= %s %s)))\n(check-sat)\n", ta, tb)
	fmt.Fprintf(&sb, "(get-value (%s %s %s))\n", ta, tb, strings.Join(names, " "))
	r, err := z.Run(ctx, sb.String())
	if err != nil {
		return Result{}, err
	}
	switch r.Status {
	case solver.Unsat:
		return Result{Equivalent: true}, nil
	case solver.Unknown:
		return Result{Unknown: true}, nil
	}
	res := Result{}
	if len(r.Replies) > 0 {
		for i, pair := range r.Replies[0].List {
			if len(pair.List) != 2 {
				continue
			}
			val := pair.List[1].String()
			switch i {
			case 0:
				res.AHolds = val == "true"
				continue
			case 1:
				res.BHolds = val == "true"
				continue
			}
			v := vars[pair.List[0].Atom]
			if v == nil {
				continue
			}
			res.Witness = append(res.Witness, checker.Binding{Name: v.Label, Value: checker.Value(v, val)})
		}
	}
	return res, nil
}
