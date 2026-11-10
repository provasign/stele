package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/graph"
	"github.com/provasign/stele/internal/ir"
)

// minRare is how strongly a rule's prose must share the request's words to
// be shown: about this many words that only one rule uses. Measured on
// Project A and two held-out registries: known conflicting pairs are
// found, and requests about nothing the specs say get no rules.
const minRare = 1.2

// constraint is one approved statement a request may touch.
type constraint struct {
	Statement string   `json:"statement"`
	Rules     []string `json:"rules"`
	Source    string   `json:"source,omitempty"`
	Prose     string   `json:"prose"`
	Approved  bool     `json:"approved"` // the owner approved every one of its rules
}

// cmdRules prints the approved rules a request touches, for the coding
// agent to keep while it plans (shift-left prevention). It reads the
// request from its arguments or stdin, prints nothing when the request
// touches nothing the specs say, and exits 0 unless it cannot read the
// registry, so a hook never blocks on it.
func cmdRules(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("rules", flag.ContinueOnError)
	project := fs.String("project", ".", "directory whose stele.yaml names the registry")
	registry := fs.String("registry", "", "registry directory (default: from stele.yaml)")
	op := fs.String("op", "", "list every approved rule about this operation, e.g. Workspace.stop")
	k := fs.Int("k", 8, "most statements to show")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := *registry
	if dir == "" {
		cfg, err := config.Find(*project)
		if err != nil || cfg == nil {
			if err == nil {
				err = fmt.Errorf("no %s in %s or its parents", config.FileName, *project)
			}
			fmt.Fprintf(os.Stderr, "stele rules: %v\n", err)
			return 2
		}
		dir = cfg.RegistryDir()
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele rules: %v\n", err)
		return 2
	}
	live := *reg
	live.Rules = nil
	// The owner's requirements: live rules translated from a spec statement,
	// not the facts Stele added to connect them.
	for _, x := range reg.Rules {
		if x.Live() && x.StatementID != "" {
			live.Rules = append(live.Rules, x)
		}
	}

	var rules []*ir.Rule
	ops := map[string]int{}
	if *op != "" {
		for _, x := range live.Rules {
			if x.AppliesTo(*op) {
				rules = append(rules, x)
			}
		}
		*k = len(rules)
	} else {
		text := strings.Join(fs.Args(), " ")
		if text == "" {
			b, _ := io.ReadAll(in)
			text = string(b)
		}
		var ms []graph.RuleMatch
		ms, ops = graph.Build(&live).Constraints(text, len(live.Rules), minRare)
		for _, m := range ms {
			rules = append(rules, m.Rule)
		}
	}
	// A registry with no more statements than fit is shown whole once the
	// request touches any of it: complete, and still short.
	if *op == "" && len(rules) > 0 {
		if all := statements(live.Rules, *k+1); len(all) <= *k {
			seen := map[*ir.Rule]bool{}
			for _, x := range rules {
				seen[x] = true
			}
			for _, x := range live.Rules {
				if !seen[x] {
					rules = append(rules, x)
				}
			}
		}
	}
	cs := statements(rules, *k)
	// An operation the request names has more rules than fit: say how to
	// see them all.
	var more []string
	for name, n := range ops {
		shown := 0
		for _, c := range cs {
			for _, id := range c.Rules {
				if x := live.Rule(id); x != nil && x.AppliesTo(name) {
					shown++
				}
			}
		}
		if n > shown {
			more = append(more, fmt.Sprintf("%s has %d approved rules in all; run `stele rules --op %s` before planning a change to it.", name, n, name))
		}
	}
	sort.Strings(more)

	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"statements": cs, "operations": ops})
		return 0
	}
	if len(cs) == 0 && len(more) == 0 {
		return 0
	}
	// "Approved" only for what the owner approved: rules written into the
	// registry without an approval are marked.
	all := true
	for _, c := range cs {
		all = all && c.Approved
	}
	if all {
		fmt.Fprintln(out, "Stele: approved requirements this request may touch, in the specs' own words:")
	} else {
		fmt.Fprintln(out, "Stele: requirements this request may touch, in the specs' own words:")
	}
	for _, c := range cs {
		where := ""
		if c.Source != "" {
			where = " (" + c.Source + ")"
		}
		mark := ""
		if !all && !c.Approved {
			mark = " [not yet approved by the owner]"
		}
		fmt.Fprintf(out, "- %s%s%s\n", plainProse(c.Prose), where, mark)
	}
	for _, m := range more {
		fmt.Fprintln(out, m)
	}
	fmt.Fprintln(out, "A plan must keep these. If the request cannot be done without changing one, stop before you edit anything, quote that requirement and ask the owner what should hold.")
	return 0
}

// statements groups rules by the statement they come from, in order, up to
// k statements.
func statements(rules []*ir.Rule, k int) []constraint {
	var out []constraint
	index := map[string]int{}
	for _, x := range rules {
		key := x.StatementID
		if key == "" {
			key = x.Prose
		}
		if key == "" {
			key = x.ID
		}
		if i, ok := index[key]; ok {
			out[i].Rules = append(out[i].Rules, x.ID)
			out[i].Approved = out[i].Approved && x.Approved()
			continue
		}
		if len(out) == k {
			continue
		}
		index[key] = len(out)
		prose := x.Prose
		if prose == "" {
			prose = x.ID
		}
		out = append(out, constraint{Statement: key, Rules: []string{x.ID}, Source: x.Source, Prose: prose, Approved: x.Approved()})
	}
	return out
}

// plainProse is a statement as one line, its requirement said once: the
// title (an OpenSpec or Kiro "Requirement:" heading) and its normative
// sentences, without markdown headings, scenarios or examples.
func plainProse(prose string) string {
	title := ""
	var body []string
	for _, line := range strings.Split(prose, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "Requirement:"):
			title = strings.TrimSpace(strings.TrimPrefix(line, "Requirement:"))
		case strings.HasPrefix(line, "Section:"), strings.HasPrefix(line, "#"), strings.HasPrefix(line, "- **"), strings.HasPrefix(line, "Scenario"):
			if strings.HasPrefix(line, "####") || strings.HasPrefix(line, "Scenario") {
				goto done // the scenarios restate the requirement
			}
		default:
			body = append(body, line)
		}
	}
done:
	if title == "" && len(body) > 1 && !strings.ContainsAny(body[0][len(body[0])-1:], ".:;!?") {
		// A heading line without a full stop (a Shale request's title).
		title, body = body[0], body[1:]
	}
	text := strings.Join(body, " ")
	if text == "" {
		text = strings.Join(strings.Fields(prose), " ")
	}
	if len(text) > 240 {
		text = text[:237] + "..."
	}
	if title != "" {
		return title + ": " + text
	}
	return text
}
