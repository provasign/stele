package adopt

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// Preview checks each item's drafted directions before the owner decides
// (ISSUES 33): retiring a rule can expose conflicts it hid, because an
// invariant on the state before a step limits the situations checked.
// For each direction that retires rules, it checks the registry without
// them and records the conflicts that were not there before. Units the
// retirement does not touch are answered from one shared cache.
func Preview(ctx context.Context, z *solver.Z3, reg *ir.Registry, base *checker.Result, items []Item) error {
	before := map[string]bool{}
	for _, c := range base.Conflicts {
		before[ruleKey(c.Rules)] = true
	}
	cache := &checker.Cache{}
	exposed := func(retire []string) ([]string, error) {
		if len(retire) == 0 {
			return nil, nil
		}
		saved := map[*ir.Rule]string{}
		for _, p := range retire {
			id, _, _ := strings.Cut(p, "@")
			if x := reg.Rule(id); x != nil {
				saved[x] = x.Status
				x.Status = "retired"
			}
		}
		defer func() {
			for x, s := range saved {
				x.Status = s
			}
		}()
		prog, err := lower.Lower(reg, lower.Options{IgnoreDecisions: true})
		if err != nil {
			return nil, err
		}
		res, err := checker.CheckWith(ctx, z, prog, checker.Options{Cache: cache})
		if err != nil {
			return nil, err
		}
		var out []string
		for _, c := range res.Conflicts {
			if before[ruleKey(c.Rules)] {
				continue
			}
			parts := make([]string, len(c.Counterexample))
			for i, b := range c.Counterexample {
				parts[i] = b.Name + "=" + b.Value
			}
			out = append(out, fmt.Sprintf("%s: no response of %s when %s", strings.Join(c.Rules, ", "), c.Unit, strings.Join(parts, ", ")))
		}
		return out, nil
	}
	for i := range items {
		var err error
		if items[i].Exposes, err = exposed(items[i].Retire); err != nil {
			return fmt.Errorf("%s: %w", items[i].ID, err)
		}
		if items[i].ExposesIfReversed, err = exposed(items[i].RetireIfReversed); err != nil {
			return fmt.Errorf("%s: %w", items[i].ID, err)
		}
	}
	return nil
}

func ruleKey(rules []string) string {
	return strings.Join(slices.Sorted(slices.Values(rules)), "\x00")
}
