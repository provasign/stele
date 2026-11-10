package change

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/equiv"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// narrower finds, for each conflict between the rules of exactly two
// statements, the statement whose condition lies inside the other's: the
// likely exception to a general rule (ISSUES 56). It returns
// "a\x00b" (sorted statement IDs) -> the narrower statement's ID.
func narrower(ctx context.Context, z *solver.Z3, headDir string, head *ir.Registry, conflicts [][]string) map[string]string {
	out := map[string]string{}
	files, err := yamlFiles(headDir)
	if err != nil {
		return out
	}
	for _, rules := range conflicts {
		by := map[string]*ir.Rule{}
		for _, id := range rules {
			if x := head.Rule(id); x != nil && x.StatementID != "" && by[x.StatementID] == nil {
				by[x.StatementID] = x
			}
		}
		if len(by) != 2 || len(by) != len(rules) {
			continue
		}
		var ids []string
		for id := range by {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		a, b := by[ids[0]], by[ids[1]]
		ab, okAB := implies(ctx, z, files, head, a, b) // a's condition inside b's
		ba, okBA := implies(ctx, z, files, head, b, a)
		switch {
		case okAB && okBA && ab && !ba:
			out[ids[0]+"\x00"+ids[1]] = ids[0]
		case okAB && okBA && ba && !ab:
			out[ids[0]+"\x00"+ids[1]] = ids[1]
		}
	}
	return out
}

// implies reports whether x's condition implies y's in every state; ok is
// false when the solver cannot tell or the rules are not comparable.
func implies(ctx context.Context, z *solver.Z3, files []string, head *ir.Registry, x, y *ir.Rule) (result, ok bool) {
	// Only conditions written as conditions: an assertion that is itself
	// conditional (a implies b, a or b) hides part of its scope from the
	// comparison, and a recommendation the wrong way round is worse than none.
	for _, r := range []*ir.Rule{x, y} {
		if b, ok := r.Assert.(*ir.Binary); ok && (b.Op == "implies" || b.Op == "or") {
			return false, false
		}
	}
	if x.Op() != y.Op() && x.Op() != "" && y.Op() != "" {
		return false, false // conditions about different steps
	}
	sx, err := head.SubjectOf(x)
	if err != nil {
		return false, false
	}
	if sx == nil && x.Op()+y.Op() == "" {
		// Two rules over the state that name no subject ("order.paid"):
		// there is no instance to compare their conditions on, and the
		// check of a change with such a pair crashed here (2026-11-10).
		return false, false
	}
	if sy, err := head.SubjectOf(y); err != nil || sy != sx {
		return false, false
	}
	cond := func(r *ir.Rule) string {
		if r.When == nil {
			return "true"
		}
		return "(" + r.When.String() + ")"
	}
	dir, err := os.MkdirTemp("", "stele-narrower-")
	if err != nil {
		return false, false
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "implies.yaml")
	// A condition may read the request, which exists only within a step.
	where := ""
	if sx != nil {
		where = "subject: " + sx.Name
	}
	if op := x.Op() + y.Op(); op != "" {
		if x.Op() != "" {
			op = x.Op()
		}
		where = "on: " + op
	}
	src := fmt.Sprintf("id: stele-implies\nkind: invariant\n%s\nassert: %q\n", where, cond(x)+" implies "+cond(y))
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
		return false, false
	}
	reg, err := ir.LoadFiles(append(append([]string{}, files...), f)...)
	if err != nil {
		debugf("load: %v", err)
		return false, false
	}
	prog, err := lower.Lower(reg, lower.Options{IgnoreDecisions: true})
	if err != nil {
		debugf("lower: %v", err)
		return false, false
	}
	var fs []*lower.Formula
	for _, u := range prog.Units {
		for _, group := range [][]*lower.Formula{u.Rules, u.Assumptions} {
			for _, fm := range group {
				if fm.Home && fm.Rule != nil && fm.Rule.ID == "stele-implies" {
					fs = append(fs, fm)
				}
			}
		}
	}
	if len(fs) == 0 {
		debugf("no formula")
		return false, false
	}
	res, err := equiv.Compare(ctx, z, prog.Datatypes, fs[:1], nil)
	if err != nil || res.Unknown {
		return false, false
	}
	return res.Equivalent, true // equivalent to true: the implication always holds
}

func yamlFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func debugf(format string, args ...any) {
	if os.Getenv("STELE_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "narrower: "+format+"\n", args...)
	}
}
