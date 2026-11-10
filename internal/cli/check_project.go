package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/provasign/stele/internal/card"
	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// clashSide is one requirement in a contradiction, as people know it.
type clashSide struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Source     string `json:"source,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
}

// clash is a set of requirements that cannot all hold.
type clash struct {
	Requirements []clashSide `json:"requirements"`
	Example      string      `json:"example,omitempty"`
}

// checkProject is `stele check` in a project, with no registry named: every
// rule in the branch's registry against every other, reported as the
// requirements they come from. Changes approved apart (two branches, then a
// merge or a pull) can contradict each other once they meet; each change was
// only checked against what its branch held.
func checkProject(cfg *config.Config, jsonOut bool, z3Path string, timeout time.Duration, out, errOut io.Writer) int {
	dir := cfg.RegistryDir()
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, dir); err == nil && !strings.HasPrefix(rel, "..") {
			dir = rel
		}
	}
	reg, prog, err := loadProgram(dir, lower.Options{})
	if err != nil {
		fmt.Fprintf(errOut, "stele check: %s: %v\n", dir, err)
		return 2
	}
	res, err := checker.CheckWith(context.Background(), &solver.Z3{Path: z3Path, Timeout: timeout}, prog, gate.CheckOptions(reg))
	if err != nil {
		fmt.Fprintf(errOut, "stele check: %v\n", err)
		return 2
	}
	gate.Pinned(res, reg)  // a value a rule fixes: its others are impossible by design
	gate.Ratchet(res, reg) // contradictions the owner accepted as open are not news

	var approved, pending []string
	for _, x := range reg.Rules {
		if !x.Live() {
			continue
		}
		id := statementOf(x)
		switch {
		case x.Approved() && !slices.Contains(approved, id):
			approved = append(approved, id)
		case !x.Approved() && !slices.Contains(pending, id):
			pending = append(pending, id)
		}
	}
	clashes := clashesOf(reg, res.Conflicts)
	next := ""
	if len(clashes) > 0 {
		next = "settle each contradiction with the owner, as the requirements say it; Stele does not choose"
		if cfg.Specs != nil && cfg.Specs.Format == "shale" {
			next = "for each, declare the request that settles it with `shale intent`, saying which holds or how they combine, and check it with `stele change shale . --base <branch>`: the owner then retires the request that is out of date"
		} else if cfg.Specs != nil {
			next = "for each, edit the requirement that is out of date in the specs, or say how the two combine, and check the change with `stele change`"
		}
	}

	if jsonOut {
		js, _ := json.MarshalIndent(struct {
			Requirements   int     `json:"requirements"`
			NotApproved    int     `json:"not_approved,omitempty"`
			Contradictions []clash `json:"contradictions"`
			Undecided      int     `json:"undecided,omitempty"`
			Next           string  `json:"next,omitempty"`
		}{len(approved), len(pending), append([]clash{}, clashes...), len(res.Undecided), next}, "", "  ")
		fmt.Fprintln(out, string(js))
	} else {
		fmt.Fprintf(out, "Stele checked the %d approved requirement(s) in %s against each other", len(approved), dir)
		if len(pending) > 0 {
			fmt.Fprintf(out, ", with %d not approved yet", len(pending))
		}
		fmt.Fprintln(out, ".")
		if len(clashes) == 0 {
			fmt.Fprintln(out, "No two contradict.")
		}
		for i, c := range clashes {
			fmt.Fprintf(out, "\n%d. ", i+1)
			for j, s := range c.Requirements {
				if j > 0 {
					fmt.Fprint(out, "   and ")
				}
				fmt.Fprintf(out, "%q", s.Title)
				var where []string
				if s.Source != "" {
					where = append(where, s.Source)
				}
				if s.ApprovedBy != "" {
					where = append(where, "approved by "+s.ApprovedBy)
				}
				if len(where) > 0 {
					fmt.Fprintf(out, " (%s)", strings.Join(where, ", "))
				}
				fmt.Fprintln(out)
			}
			verb := "   cannot all hold"
			switch len(c.Requirements) {
			case 2:
				verb = "   cannot both hold"
			case 1:
				verb = "   contradicts itself: Stele's reading of it may be wrong"
			}
			if c.Example != "" {
				verb += ", for example when " + c.Example
			}
			fmt.Fprintln(out, verb+".")
		}
		if n := len(res.Undecided); n > 0 {
			fmt.Fprintf(out, "\n%d part(s) could not be decided (%s), so contradictions there would not be found; a longer --timeout sometimes settles them, and a simpler wording of the requirements on those steps more often does.\n", n, strings.Join(res.Undecided, ", "))
		}
		if next != "" {
			fmt.Fprintf(out, "\nNext: %s.\n", next)
		}
	}
	switch {
	case len(clashes) > 0:
		return 1
	case len(res.Undecided) > 0:
		return 3
	}
	return 0
}

// clashesOf turns the checker's conflicts into contradictions between
// requirements: the rules of one requirement count once, and conflicts
// between the same requirements are one contradiction.
func clashesOf(reg *ir.Registry, conflicts []checker.Conflict) []clash {
	seen := map[string]bool{}
	var out []clash
	for _, cf := range conflicts {
		var ids []string
		sides := map[string]clashSide{}
		for _, rid := range cf.Rules {
			x := reg.Rule(rid)
			if x == nil {
				continue // an operation's declared effect, not a requirement
			}
			id := statementOf(x)
			if _, ok := sides[id]; ok {
				continue
			}
			ids = append(ids, id)
			sides[id] = clashSide{ID: id, Title: card.Title(x.Prose, id), Source: x.Source, ApprovedBy: x.ApprovedBy}
		}
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		key := strings.Join(ids, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		c := clash{Example: checker.Plain(checker.Situation(reg, cf.Counterexample))}
		for _, id := range ids {
			c.Requirements = append(c.Requirements, sides[id])
		}
		out = append(out, c)
	}
	return out
}

// statementOf is the requirement a rule was translated from, or the rule
// itself when it was written by hand.
func statementOf(x *ir.Rule) string {
	if x.StatementID != "" {
		return x.StatementID
	}
	return x.ID
}
