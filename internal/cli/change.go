package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/change"
	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/formalize"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// cmdChange checks one OpenSpec change folder against the approved
// registry before the change is applied (D-057).
func cmdChange(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("change", flag.ExitOnError)
	registry := fs.String("registry", "", "the approved registry (default: registry in stele.yaml)")
	outDir := fs.String("out", "", "work directory (default .stele/change/<id>)")
	stmtsPath := fs.String("statements", "", "the statements the registry was built from (default: import openspec/specs)")
	vocab := fs.String("vocab", "", "comma-separated vocabulary files for translation (default: the registry's files that hold no statement rule)")
	list := fs.Bool("list", false, "list the active change folders and stop")
	asJSON := fs.Bool("json", false, "print the questions and status as JSON, for a coding agent (D-058)")
	apply := fs.Bool("apply", false, "when the check finds no conflict, write the change's rules into the registry (still to be approved)")
	timeout := fs.Duration("timeout", 30*time.Minute, "limit for the run")
	requestTimeout := fs.Duration("request-timeout", 3*time.Minute, "limit per model call: a call that stalls fails and is retried, and a requirement whose calls keep failing is reported under failed")
	base := fs.String("base", "", "compare the specs at this git ref with the working tree (a feature branch); required for speckit, kiro and shale")
	noCode := fs.Bool("no-code", false, "do not ask what the code does for each conflict (D-062)")
	graph := fs.Bool("graph", false, "also find definitions in Grove's code graph (no gain measured on 2026-10-29)")
	maxCost := fs.Float64("max-cost", 0, "spend at most this many dollars on the translation's model calls (as the backend reports); a run stopped by it keeps its work for the next run; 0 is unlimited")
	client := llmFlags(fs)
	if len(args) > 0 && args[0] == "answer" {
		return cmdChangeAnswer(args[1:], out)
	}
	if len(args) > 0 && args[0] == "reread" {
		return cmdChangeReread(args[1:], out)
	}
	usage := "usage: stele change openspec <repo> [<change-id>] [--registry DIR] [--out DIR] [--json] [--apply] [--list] [--llm B --model M]\n" +
		"       stele change speckit|kiro <repo> [<name>] --base REF [--registry DIR] [--json] ...   (a feature branch against REF)\n" +
		"       stele change shale <repo> [<name>] --base REF [--json] ...   (the requests declared with `shale intent` on this branch)\n" +
		"       stele change answer <change-id> <question-id> <choice> [--note TEXT] [--by NAME]\n" +
		"       stele change reread <change-id> <requirement> --note TEXT   (the owner says Stele misread it)\n"
	// `stele change shale --help` read --help as the repository and then
	// asked for --base.
	if slices.ContainsFunc(args, func(a string) bool { return a == "-h" || a == "-help" || a == "--help" }) {
		fmt.Fprint(out, usage+"\nflags:\n")
		fs.SetOutput(out)
		fs.PrintDefaults()
		return 0
	}
	if len(args) < 2 || !slices.Contains([]string{"openspec", "speckit", "kiro", "shale", adoptChange}, args[0]) {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	format, repo := args[0], args[1]
	id := ""
	rest := args[2:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	_ = fs.Parse(rest)
	var adoptStmts []translate.Statement
	adoptDir := ""
	if format == adoptChange {
		// Adoption: the project's whole specs, in the layout stele.yaml
		// records, are one change against the empty registry.
		cfg, err := config.Find(repo)
		if err != nil || cfg == nil {
			if err == nil {
				err = fmt.Errorf("no %s here: run `stele init` first", config.FileName)
			}
			return failf("adopt", err)
		}
		sp := cfg.Specs
		if sp == nil {
			if sp = detectSpecs(repo); sp == nil {
				return failf("adopt", fmt.Errorf("no specs found (openspec/, .kiro/specs/, .specify/, specs/ or docs/); name them under specs: in %s", config.FileName))
			}
		}
		if reg, err := ir.LoadDir(cfg.RegistryDir()); err == nil && len(reg.Rules) > 0 {
			return failf("adopt", fmt.Errorf("the registry already has rules: check spec changes with `stele change` instead"))
		}
		if adoptStmts, err = importSpecs(repo, sp); err != nil {
			return failf("adopt", err)
		}
		if len(adoptStmts) == 0 {
			return failf("adopt", fmt.Errorf("no requirements found in the %s specs", formatLabel(sp.Format)))
		}
		format, id, adoptDir = sp.Format, adoptChange, filepath.Join(repo, sp.Root)
		if sp.Format != "markdown" {
			adoptDir = filepath.Join(repo, specRootOf(sp.Format))
		}
	} else if format != "openspec" && *base == "" {
		fmt.Fprint(os.Stderr, "stele change "+format+": name the branch to compare with, e.g. --base main\n")
		return 2
	}
	named := id != ""
	if *list {
		ids, err := importer.OpenSpecChanges(repo)
		if err != nil {
			return failf("change", err)
		}
		for _, c := range ids {
			fmt.Fprintln(out, c)
		}
		return 0
	}
	// stele.yaml in the repository gives the registry and the model.
	if cfg, err := config.Find(repo); err != nil {
		return failf("change", err)
	} else if cfg != nil {
		if *registry == "" && cfg.Registry != "" {
			*registry = cfg.RegistryDir()
		}
		if !flagSet(fs, "llm") && !flagSet(fs, "llm-cmd") && cfg.Translate.LLM != "" {
			_ = fs.Lookup("llm").Value.Set(cfg.Translate.LLM)
		}
		if !flagSet(fs, "model") && cfg.Translate.Model != "" {
			_ = fs.Lookup("model").Value.Set(cfg.Translate.Model)
		}
	}
	if *base != "" && !named {
		id = branchChangeName(repo, *registry)
	}
	if id == "" || *registry == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if *outDir == "" {
		*outDir = filepath.Join(".stele", "change", id)
	}
	opt := change.Options{Repo: repo, Change: id, Registry: *registry, Out: *outDir, Z3: &solver.Z3{}, Log: os.Stderr, Base: *base, Format: format, NoCode: *noCode, Graph: *graph, Adopt: adoptStmts, SpecsDir: adoptDir, MaxCost: *maxCost, RequestTimeout: *requestTimeout}
	if *stmtsPath != "" {
		stmts, err := translate.LoadStatements(*stmtsPath)
		if err != nil {
			return failf("change", err)
		}
		opt.Truth = stmts
	}
	if *vocab != "" {
		opt.VocabFiles = strings.Split(*vocab, ",")
	}
	m, err := client()
	if err != nil {
		return failf("change", err)
	}
	opt.Clients = append(opt.Clients, m)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := change.Run(ctx, opt)
	if err != nil {
		if *asJSON {
			// An agent reads the JSON: a run that stopped must still say so
			// there, not only on stderr.
			js, _ := json.MarshalIndent(changeSummary{Change: id, Status: "failed", Error: err.Error(), Questions: []change.Question{},
				Next: "the check stopped before it finished (see error); nothing new was checked. Fix what the error names, or raise --timeout, and run the same check again: translations already done are kept"}, "", "  ")
			fmt.Fprintln(out, string(js))
			return 1
		}
		return failf("change", err)
	}
	if res.Approved {
		if *asJSON {
			js, _ := json.MarshalIndent(changeSummary{Change: res.Change, Status: "approved", Next: "nothing to decide: the owner approved this change as it reads; commit the registry with it", Questions: []change.Question{}}, "", "  ")
			fmt.Fprintln(out, string(js))
		} else {
			fmt.Fprintf(out, "%s is approved as it reads; commit the registry with it.\n", res.Change)
		}
		return 0
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		return failf("change", err)
	}
	report := changeReport(res, head)
	if err := os.WriteFile(filepath.Join(*outDir, "report.md"), []byte(report), 0o644); err != nil {
		return failf("change", err)
	}
	js, _ := json.MarshalIndent(res, "", "  ")
	_ = os.WriteFile(filepath.Join(*outDir, "result.json"), append(js, '\n'), 0o644)
	if err := writeCard(res, head, summarize(res, *outDir).Status, *base); err != nil {
		fmt.Fprintf(os.Stderr, "stele change: could not write the PR card: %v\n", err)
	}
	blocked := len(res.Gate.Errors) > 0 || res.Gate.Check == nil || !res.Gate.Accepted || len(res.Failed) > 0
	if *asJSON {
		js, _ := json.MarshalIndent(summarize(res, *outDir), "", "  ")
		fmt.Fprintln(out, string(js))
		if blocked {
			return 1
		}
		return 0
	}
	fmt.Fprint(out, report)
	if *apply {
		if blocked {
			fmt.Fprintln(out, "not applied: resolve the conflicts first")
			return 1
		}
		if err := copyDir(res.Head, *registry); err != nil {
			return failf("change", err)
		}
		fmt.Fprintf(out, "applied to %s; approve the new rules with `stele approve --registry %s --base <main registry> --by <you>`\n", *registry, *registry)
	}
	if blocked {
		return 1
	}
	return 0
}

// changeSummary is what `stele change --json` prints for a coding agent:
// the status, and the questions to put to the owner (D-058).
type changeSummary struct {
	Change string `json:"change"`
	// Status is approved (the owner approved the change as it reads),
	// broken (the registry after the change does not compile),
	// questions (something needs deciding), ready (only the approval is
	// left) or clean (nothing to approve).
	Status    string            `json:"status"`
	Next      string            `json:"next"`
	Questions []change.Question `json:"questions"`
	Errors    []string          `json:"errors,omitempty"`
	Added     []string          `json:"added,omitempty"`
	Modified  []string          `json:"modified,omitempty"`
	Removed   []string          `json:"removed,omitempty"`
	Unchecked map[string]string `json:"unchecked,omitempty"`
	Failed    map[string]string `json:"failed,omitempty"`
	Error     string            `json:"error,omitempty"`
	NewTerms  []string          `json:"new_terms,omitempty"`
	Reused    int               `json:"reused_translations"`
	CostUSD   float64           `json:"cost_usd"`
	Report    string            `json:"report"`
}

func summarize(res *change.Result, outDir string) changeSummary {
	s := changeSummary{Change: res.Change, Questions: res.Questions, Errors: res.Gate.Errors, Added: res.Added, Modified: res.Modified, Removed: res.Removed,
		Unchecked: res.Unchecked, Failed: res.Failed, NewTerms: res.NewTerms, Reused: len(res.Reused), CostUSD: res.Usage.CostUSD, Report: filepath.Join(outDir, "report.md")}
	if s.Questions == nil {
		s.Questions = []change.Question{}
	}
	approve := len(res.Questions) == 1 && res.Questions[0].Kind == "approve"
	switch {
	case len(res.Gate.Errors) > 0:
		s.Status, s.Next = "broken", "the registry after this change does not compile; see errors"
	case len(res.Failed) > 0:
		s.Status, s.Next = "failed", fmt.Sprintf("the model could not translate %d requirement(s), so nothing was checked for them (see failed); fix what the error names (the model command, its login or sandbox) and run the same check again: it retries them", len(res.Failed))
	case approve:
		s.Status, s.Next = "ready", "ask the owner the approval question; answer with `stele change answer`"
	case len(res.Questions) > 0:
		s.Status, s.Next = "questions", "answer the questions with ask_owner false yourself; ask the owner the others in their words; record each with `stele change answer`, act on its result, then run `stele change` again"
	case res.Gate.Accepted:
		s.Status, s.Next = "clean", "nothing to decide or approve"
	default:
		s.Status, s.Next = "broken", "the check did not finish; see the report"
	}
	if res.Change == adoptChange {
		s.Next = forAdoption(s.Next)
		for i, q := range s.Questions {
			opts := slices.Clone(q.Options)
			for k := range opts {
				opts[k].Then = forAdoption(opts[k].Then)
			}
			s.Questions[i].Options = opts
		}
	}
	return s
}

// forAdoption names adoption's own command where a next step says to run
// the change check again.
func forAdoption(next string) string {
	return strings.ReplaceAll(next, "run `stele change` again", "run `stele adopt` again")
}

// changeReport renders a change check in OpenSpec's terms: capabilities
// and requirement titles, marked by what the change does to them.
func changeReport(res *change.Result, head *ir.Registry) string {
	mark := map[string]string{}
	for _, id := range res.Added {
		mark[id] = "ADDED in this change"
	}
	for _, id := range res.Modified {
		mark[id] = "MODIFIED in this change"
	}
	stmtOf := func(ruleID string) string {
		if x := head.Rule(ruleID); x != nil && x.StatementID != "" {
			return x.StatementID
		}
		return ""
	}
	name := func(ruleID string) string {
		x := head.Rule(ruleID)
		stmt := stmtOf(ruleID)
		if stmt == "" {
			prose := ""
			if x != nil {
				prose = ": " + oneLine(strings.TrimPrefix(x.Prose, "World fact: "), 160)
			}
			return fmt.Sprintf("world fact %s%s", ruleID, prose)
		}
		label := stmt
		if c, title, ok := strings.Cut(stmt, "/"); ok {
			label = fmt.Sprintf("%s › Requirement: %s", c, title)
		}
		where := mark[stmt]
		if where == "" {
			where = "current spec"
		}
		if src := res.Sources[stmt]; src != "" {
			where += ", " + src
		}
		return fmt.Sprintf("%s (%s)\n  - rule `%s`: %s", label, where, ruleID, ruleEnglish(x))
	}
	g := res.Gate
	var b strings.Builder
	fmt.Fprintf(&b, "# Change %s\n\n", res.Change)
	fmt.Fprintf(&b, "%d added, %d modified, %d removed, %d renamed requirement(s).", len(res.Added), len(res.Modified), len(res.Removed), len(res.Renamed))
	rules := 0
	for _, ids := range res.Rules {
		rules += len(ids)
	}
	fmt.Fprintf(&b, " Translated into %d rule(s); %d rule(s) replaced, %d retired.", rules, len(res.Replaced), len(res.Retired))
	if res.Usage.CostUSD > 0 {
		fmt.Fprintf(&b, " About $%.2f.", res.Usage.CostUSD)
	}
	b.WriteString("\n\n")
	switch {
	case len(g.Errors) > 0:
		b.WriteString("**Result: the registry after this change does not compile.**\n\n")
		for _, e := range g.Errors {
			fmt.Fprintf(&b, "- %s\n", e)
		}
		b.WriteString("\n")
	case g.Accepted:
		b.WriteString("**Result: no conflict with the approved rules.**\n\n")
	default:
		b.WriteString("**Result: conflicts with the approved rules.**\n\n")
	}
	if g.Check != nil {
		for i, c := range g.Check.Conflicts {
			stmts := map[string]bool{}
			for _, r := range c.Rules {
				stmts[stmtOf(r)] = true
			}
			fmt.Fprintf(&b, "## Conflict %d\n\n", i+1)
			if len(stmts) == 1 && !stmts[""] {
				b.WriteString("Rules translated from one requirement contradict each other. This usually means the translation is wrong, not the requirement: check these rules before approving them")
			} else {
				b.WriteString("These requirements cannot all hold")
			}
			if s := situation(head, c.Counterexample); len(s) > 0 {
				fmt.Fprintf(&b, " (for example when %s)", strings.Join(s, ", "))
			}
			b.WriteString(":\n\n")
			for _, r := range c.Rules {
				fmt.Fprintf(&b, "- %s\n", name(r))
			}
			if len(stmts) > 1 || stmts[""] {
				b.WriteString("\nFix the change, update the older requirement in this change (a MODIFIED delta for its capability), or record a decision.\n")
			}
			b.WriteString("\n")
		}
		for _, d := range g.Check.Dead {
			if d.NeverAllowed {
				fmt.Fprintf(&b, "## %s never runs\n\nIts default is deny and no rule allows it. Give the operation `default: allow` (a deny rule then forbids it only where it says), or add the rule that allows it.\n\n", d.Unit)
				continue
			}
			fmt.Fprintf(&b, "## %s can never happen\n\n", d.Response)
			if len(d.Rules) == 0 {
				b.WriteString("No rule is involved: the declared ranges, the operation's default, or the invariants about the state before rule it out.\n")
			}
			for _, r := range d.Rules {
				fmt.Fprintf(&b, "- %s\n", name(r))
			}
			b.WriteString("\n")
		}
		if len(g.Check.Undecided) > 0 {
			fmt.Fprintf(&b, "The solver could not decide: %s.\n\n", strings.Join(g.Check.Undecided, ", "))
		}
		if len(g.Check.NearLimit) > 0 {
			fmt.Fprintf(&b, "Decided only near the solver timeout, so a slower machine would not decide them: %s.\n\n", strings.Join(g.Check.NearLimit, ", "))
		}
	}
	if len(res.NewTerms) > 0 || len(res.SameTerms) > 0 {
		b.WriteString("## New terms\n\n")
		if len(res.NewTerms) > 0 {
			fmt.Fprintf(&b, "The change adds: %s.\n", strings.Join(res.NewTerms, ", "))
		}
		for _, mg := range res.SameTerms {
			fmt.Fprintf(&b, "- %s was read as the existing %s (%s)\n", mg.From, mg.Into, mg.Reason)
		}
		b.WriteString("\n")
	}
	if len(res.Questions) > 0 {
		b.WriteString("## Questions for the owner\n\n")
		for _, q := range res.Questions {
			fmt.Fprintf(&b, "- `%s` %s\n", q.ID, q.Text)
			for _, o := range q.Options {
				fmt.Fprintf(&b, "  - `%s`: %s\n", o.Choice, o.Label)
			}
		}
		b.WriteString("\n")
	}
	if len(res.Failed) > 0 {
		b.WriteString("## Not translated\n\nThe model could not translate these requirements, so nothing was checked for them and the change cannot be approved yet. Fix what the error names and run the same check again; it retries them:\n\n")
		for _, id := range sortedKeys(res.Failed) {
			fmt.Fprintf(&b, "- %s: %s\n", id, oneLine(res.Failed[id], 200))
		}
		b.WriteString("\n")
	}
	if len(res.Unchecked) > 0 {
		b.WriteString("## Not checked\n\nThese requirements became no checked rule, so the check says nothing about them:\n\n")
		for _, id := range sortedKeys(res.Unchecked) {
			fmt.Fprintf(&b, "- %s: %s\n", id, oneLine(res.Unchecked[id], 200))
		}
		b.WriteString("\n")
	}
	if len(g.Unapproved) > 0 {
		var checked, informal []string
		for _, id := range g.Unapproved {
			if x := head.Rule(id); x != nil && (x.Kind == ir.Informal || x.Kind == ir.Obligation) {
				informal = append(informal, id)
			} else {
				checked = append(checked, id)
			}
		}
		b.WriteString("## Rules to approve\n\nRead each rule as a reading of its requirement. The owner approves them by answering the check's approval question (`stele change answer <change> <approve question> approve`), which applies the change and records the approval.\n\n")
		byStmt := map[string][]string{}
		var stmts []string
		for _, id := range checked {
			s := head.Rule(id).StatementID
			if byStmt[s] == nil {
				stmts = append(stmts, s)
			}
			byStmt[s] = append(byStmt[s], id)
		}
		for _, s := range stmts {
			fmt.Fprintf(&b, "### %s\n\n", s)
			if text := res.Statements[s]; text != "" {
				fmt.Fprintf(&b, "> %s\n\n", oneLine(text, 600))
			}
			for _, id := range byStmt[s] {
				fmt.Fprintf(&b, "- **%s**: %s\n", id, ruleEnglish(head.Rule(id)))
			}
			for _, f := range res.Findings[s] {
				fmt.Fprintf(&b, "- ⚠ %s\n", oneLine(f, 300))
			}
			b.WriteString("\n")
		}
		if len(informal) > 0 {
			fmt.Fprintf(&b, "\nRecorded as informal or obligation, so reviewed rather than checked: %s.\n", strings.Join(informal, ", "))
		}
		b.WriteString("\n")
	}
	if len(res.Retired) > 0 {
		fmt.Fprintf(&b, "Retired with this change: %s.\n\n", strings.Join(res.Retired, ", "))
	}
	if len(res.NewLessons) > 0 {
		b.WriteString("## Lessons this change saves\n\nWhat the owner's answers teach Stele about this project, written to the registry's lessons/ and committed with the change (D-063):\n\n")
		for _, l := range res.NewLessons {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		b.WriteString("\n")
	}
	if len(res.Lessons) > 0 {
		var lines []string
		for _, s := range sortedKeys2(res.Lessons) {
			lines = append(lines, fmt.Sprintf("%s (%s)", s, strings.Join(res.Lessons[s], ", ")))
		}
		fmt.Fprintf(&b, "Read with the owner's lessons: %s.\n\n", strings.Join(lines, "; "))
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	return b.String()
}

// ReviewItem is one requirement in an approval packet: its text, how Stele
// read it, and what Stele's own checks found in that reading.
type ReviewItem struct {
	Statement string       `json:"statement"`
	Text      string       `json:"text,omitempty"`
	Rules     []ReviewRule `json:"rules"`
	Findings  []string     `json:"findings,omitempty"`
}

// ReviewRule is one rule to approve, with the reading it replaces.
type ReviewRule struct {
	ID      string `json:"id"`
	Reading string `json:"reading"`
	Was     string `json:"was,omitempty"` // the base registry's reading, when it differs
}

// reviewPacket groups the rules to approve by requirement. Findings come
// from the formalize report in the registry directory, when there is one.
func reviewPacket(reg, base *ir.Registry, ids []string, dir string) []ReviewItem {
	findings := map[string][]string{}
	if src, err := os.ReadFile(filepath.Join(dir, formalize.ReportFile)); err == nil {
		var rep struct {
			Ends []formalize.End `json:"statements"`
		}
		if json.Unmarshal(src, &rep) == nil {
			for _, e := range rep.Ends {
				for _, u := range e.Uncovered {
					findings[e.ID] = append(findings[e.ID], fmt.Sprintf("no rule checks %q", u))
				}
				for _, b := range e.Broader {
					findings[e.ID] = append(findings[e.ID], "broader than the text: "+b)
				}
				if e.Blocks != "" {
					findings[e.ID] = append(findings[e.ID], "blocks a step: "+e.Blocks)
				}
			}
		}
	}
	var out []ReviewItem
	at := map[string]int{}
	for _, id := range ids {
		x := reg.Rule(id)
		s := x.StatementID
		if s == "" {
			s = id
		}
		k, ok := at[s]
		if !ok {
			k = len(out)
			at[s] = k
			out = append(out, ReviewItem{Statement: s, Text: x.Prose, Findings: findings[s]})
		}
		r := ReviewRule{ID: id, Reading: ruleEnglish(x)}
		if base != nil {
			if b := base.Rule(id); b != nil && b.Reading() != x.Reading() {
				r.Was = ruleEnglish(b)
			}
		}
		out[k].Rules = append(out[k].Rules, r)
	}
	return out
}

// ruleEnglish is a short plain reading of a rule for the owner.
func ruleEnglish(x *ir.Rule) string {
	if x == nil {
		return ""
	}
	var parts []string
	switch x.Kind {
	case ir.Permission, ir.Precondition:
		parts = append(parts, fmt.Sprintf("%s %s %s", x.Kind, x.Effect, x.Operation))
	default:
		parts = append(parts, x.Kind)
		if x.On != "" {
			parts = append(parts, "on "+x.On)
		}
	}
	if x.When != nil {
		parts = append(parts, "when "+x.When.String())
	}
	if x.Assert != nil {
		parts = append(parts, "then "+x.Assert.String())
	}
	return strings.Join(parts, " ")
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, src, 0o644)
	})
}

// cmdApprove records the owner's approval of rules, pinned to the text of
// the statements they were translated from (D-056).
func cmdApprove(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	registry := fs.String("registry", "", "registry holding the rules")
	rules := fs.String("rules", "", "comma-separated rule IDs to approve")
	base := fs.String("base", "", "with no --rules: approve every rule that differs from this registry (the target branch's)")
	all := fs.Bool("all", false, "with no --rules and no --base: approve every live rule not yet approved")
	by := fs.String("by", "", "who approves (the owner)")
	listOnly := fs.Bool("list", false, "list the rules that need approval, grouped by requirement with Stele's reading and what its checks found, and stop")
	asJSON := fs.Bool("json", false, "with --list: emit the review packet as JSON, for an agent to put to the owner")
	_ = fs.Parse(args)
	if *registry == "" || (*by == "" && !*listOnly) {
		fmt.Fprint(os.Stderr, "usage: stele approve --registry DIR (--rules A,B | --base DIR | --all) --by NAME [--list]\n")
		return 2
	}
	reg, err := ir.LoadDir(*registry)
	if err != nil {
		return failf("approve", err)
	}
	var ids []string
	var baseReg *ir.Registry
	switch {
	case *rules != "":
		for _, id := range strings.Split(*rules, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	case *base != "":
		b, err := ir.LoadDir(*base)
		if err != nil {
			return failf("approve", err)
		}
		baseReg = b
		ids = gate.Unapproved(b, reg)
	case *all || *listOnly:
		ids = gate.Unapproved(nil, reg)
	default:
		fmt.Fprint(os.Stderr, "stele approve: name the rules (--rules), a base registry (--base) or --all\n")
		return 2
	}
	if *listOnly {
		packet := reviewPacket(reg, baseReg, ids, *registry)
		if *asJSON {
			js, _ := json.MarshalIndent(packet, "", "  ")
			fmt.Fprintln(out, string(js))
			return 0
		}
		for _, p := range packet {
			fmt.Fprintf(out, "%s\n  text: %s\n", p.Statement, oneLine(p.Text, 400))
			for _, r := range p.Rules {
				fmt.Fprintf(out, "  %s: %s\n", r.ID, r.Reading)
				if r.Was != "" {
					fmt.Fprintf(out, "    was: %s\n", r.Was)
				}
			}
			for _, f := range p.Findings {
				fmt.Fprintf(out, "  ! %s\n", oneLine(f, 300))
			}
		}
		fmt.Fprintf(out, "%d rule(s) of %d requirement(s) need approval\n", len(ids), len(packet))
		return 0
	}
	for _, id := range ids {
		if reg.Rule(id) == nil {
			return failf("approve", fmt.Errorf("no rule %s", id))
		}
	}
	if err := adopt.ApproveRules(reg, ids, *by); err != nil {
		return failf("approve", err)
	}
	fmt.Fprintf(out, "approved %d rule(s) as %s\n", len(ids), *by)
	return 0
}

// branchChangeName names a branch change after the checked-out branch.
// Once the owner approved that change, its files are in the registry, and
// the next change on the branch takes the next free name, <branch>-2 and
// on, so it never writes over them.
func branchChangeName(repo, registry string) string {
	name := branchName(repo)
	taken := func(id string) bool {
		m, _ := filepath.Glob(filepath.Join(registry, change.RegistryPrefix(id)+".*"))
		return len(m) > 0
	}
	if registry == "" || !taken(name) {
		return name
	}
	for n := 2; ; n++ {
		if id := fmt.Sprintf("%s-%d", name, n); !taken(id) {
			return id
		}
	}
}

// branchName names a branch change after the checked-out branch.
func branchName(repo string) string {
	if b, err := exec.Command("git", "-C", repo, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		if name := strings.TrimSpace(string(b)); name != "" && name != "HEAD" {
			return strings.NewReplacer("/", "-", " ", "-").Replace(name)
		}
	}
	return "working-tree"
}

func sortedKeys2(m map[string][]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// adoptChange names the change that adopts a project's specs.
const adoptChange = "adopt"

// specRootOf is where a spec format keeps its specs, under the repository.
func specRootOf(format string) string {
	switch format {
	case "kiro":
		return filepath.Join(".kiro", "specs")
	case "openspec":
		return filepath.Join("openspec", "specs")
	case "shale":
		return ".shale"
	}
	return "specs"
}
