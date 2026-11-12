package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/card"
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
	allSpecs := fs.Bool("all", false, "stele adopt: read every spec of a large project at once, without being offered a part first")
	specFlag := fs.String("spec", "", "stele adopt: read only these specs (names separated by commas) and leave the others for a later `stele adopt --spec`")
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
	if len(args) > 0 && args[0] == "summary" {
		return cmdChangeSummary(args[1:], out)
	}
	usage := "usage: stele change openspec <repo> [<change-id>] [--registry DIR] [--out DIR] [--json] [--apply] [--list] [--llm B --model M]\n" +
		"       stele change speckit|kiro <repo> [<name>] --base REF [--registry DIR] [--json] ...   (a feature branch against REF)\n" +
		"       stele change markdown <repo> [<name>] [--json] ...   (the Markdown requirements stele.yaml names, against what is approved)\n" +
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
	if len(args) < 2 || !slices.Contains([]string{"openspec", "speckit", "kiro", "shale", "markdown", adoptChange}, args[0]) {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	format, repo := args[0], args[1]
	adopting := format == adoptChange // format becomes the specs' own below
	id := ""
	rest := args[2:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	_ = fs.Parse(rest)
	var adoptStmts []translate.Statement
	var adoptSpecs, unreadSpecs []string
	adoptName := ""
	adoptDir, anyCase := "", false
	if format == "markdown" {
		// The folder is the project's own: stele.yaml names it.
		cfg, err := config.Find(repo)
		if err != nil || cfg == nil || cfg.Specs == nil || cfg.Specs.Format != "markdown" {
			if err == nil {
				err = fmt.Errorf("%s names no Markdown requirements folder here (specs: root and format: markdown); run `stele init` to set one up", config.FileName)
			}
			return failf("change", err)
		}
		adoptDir, anyCase = cfg.SpecsDir(), cfg.Specs.AnyCase
	}
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
		if adoptStmts, err = importSpecs(repo, sp); err != nil {
			return failf("adopt", err)
		}
		if len(adoptStmts) == 0 {
			return failf("adopt", fmt.Errorf("no requirements found in the %s specs", formatLabel(sp.Format)))
		}
		// The specs may be adopted a few at a time (--spec): a first
		// adoption of 179 requirements is ten minutes and several rounds
		// of questions, one of 76 is none for the owner in its first
		// round (2026-11-10). The registry records which specs were read.
		read := readSpecs(cfg.RegistryDir())
		var names []string
		for _, s := range adoptStmts {
			if n := specName(s.ID); n != "" && !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			if !slices.Contains(read, n) {
				unreadSpecs = append(unreadSpecs, n)
			}
		}
		hasRules := false
		if reg, err := ir.LoadDir(cfg.RegistryDir()); err == nil && len(reg.Rules) > 0 {
			hasRules = true
		}
		if *list {
			// The specs and how much each holds, with no model call: what
			// the owner chooses from when adopting a part at a time.
			for _, n := range names {
				count := 0
				for _, s := range adoptStmts {
					if specName(s.ID) == n {
						count++
					}
				}
				state := "not read yet"
				if slices.Contains(read, n) || hasRules && len(read) == 0 {
					state = "read"
				}
				fmt.Fprintf(out, "%s\t%d requirement(s)\t%s\n", n, count, state)
			}
			return 0
		}
		if *specFlag == "" && !*allSpecs && !hasRules && len(names) > 1 && len(adoptStmts) > largeAdoption && !dirExists(filepath.Join(repo, ".stele", "change", adoptChange, "translated")) {
			// A large project is offered a part first, before anything is
			// spent: a coding agent read 179 requirements at once and its
			// owner heard nothing for 42 minutes, where one spec of 46 is
			// about five (2026-11-10).
			return chooseSpecs(out, *asJSON, names, adoptStmts)
		}
		switch {
		case *specFlag == "" && hasRules && len(read) > 0 && len(unreadSpecs) > 0:
			return failf("adopt", fmt.Errorf("%d spec(s) are not read yet: %s. Read one or more with `stele adopt --spec <name>[,<name>]`", len(unreadSpecs), strings.Join(unreadSpecs, ", ")))
		case *specFlag == "" && hasRules:
			return failf("adopt", fmt.Errorf("the registry already has rules: check spec changes with `stele change` instead"))
		case *specFlag != "":
			for _, n := range strings.Split(*specFlag, ",") {
				n = strings.TrimSpace(n)
				switch {
				case n == "":
				case !slices.Contains(names, n):
					return failf("adopt", fmt.Errorf("no spec named %q; the specs are: %s", n, strings.Join(names, ", ")))
				case slices.Contains(read, n):
					return failf("adopt", fmt.Errorf("the spec %q is already read: check changes to it with `stele change`", n))
				case hasRules && len(read) == 0:
					return failf("adopt", fmt.Errorf("the registry already has rules for the whole project: check spec changes with `stele change` instead"))
				case !slices.Contains(adoptSpecs, n):
					adoptSpecs = append(adoptSpecs, n)
				}
			}
			if len(adoptSpecs) == 0 {
				return failf("adopt", fmt.Errorf("--spec names no spec; the specs are: %s", strings.Join(names, ", ")))
			}
			sort.Strings(adoptSpecs)
			adoptStmts = slices.DeleteFunc(adoptStmts, func(s translate.Statement) bool { return !slices.Contains(adoptSpecs, specName(s.ID)) })
			unreadSpecs = slices.DeleteFunc(unreadSpecs, func(n string) bool { return slices.Contains(adoptSpecs, n) })
			adoptName = partName(adoptSpecs)
		default:
			unreadSpecs = nil // the whole project, at once
		}
		format, id, adoptDir = sp.Format, adoptChange, filepath.Join(repo, sp.Root)
		if sp.Format != "markdown" {
			adoptDir = filepath.Join(repo, specRootOf(sp.Format))
		}
	} else if format != "openspec" && format != "markdown" && *base == "" {
		fmt.Fprint(os.Stderr, "stele change "+format+": name the branch to compare with, e.g. --base main\n")
		return 2
	}
	if cfg, err := config.Find(repo); err == nil && cfg != nil && !adopting && !*list && unadopted(cfg, repo) {
		// The change alone, checked against an empty registry, would become
		// the registry, and the specs written before it would never be read.
		msg := "this project's specs are not adopted yet, and a change is checked against the approved requirements"
		next := "run `stele adopt --json` first: it reads every requirement once, this change's with them, and asks the owner only what needs a person"
		if *asJSON {
			js, _ := json.MarshalIndent(changeSummary{Change: id, Status: "failed", Error: msg, Questions: []change.Question{}, Next: next}, "", "  ")
			fmt.Fprintln(out, string(js))
			return 1
		}
		return failf("change", fmt.Errorf("%s; %s", msg, next))
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
		if !flagSet(fs, "effort") && cfg.Translate.Effort != "" {
			_ = fs.Lookup("effort").Value.Set(cfg.Translate.Effort)
		}
	}
	if (*base != "" || format == "markdown") && !named {
		id = branchChangeName(repo, *registry)
	}
	if id == "" || *registry == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if *outDir == "" {
		*outDir = filepath.Join(".stele", "change", id)
	}
	if adopting {
		// Another part's answers and readings are not this part's.
		mark := filepath.Join(*outDir, "part")
		if was, err := os.ReadFile(mark); (err == nil && string(was) != adoptName) || (err != nil && adoptName != "" && dirExists(*outDir)) {
			if err := os.RemoveAll(*outDir); err != nil {
				return failf("adopt", err)
			}
		}
		if adoptName != "" {
			_ = os.MkdirAll(*outDir, 0o755)
			_ = os.WriteFile(mark, []byte(adoptName), 0o644)
		}
	}
	opt := change.Options{Repo: repo, Change: id, Registry: *registry, Out: *outDir, Z3: &solver.Z3{}, Log: os.Stderr, Base: *base, Format: format, NoCode: *noCode, Graph: *graph, Adopt: adoptStmts, AdoptName: adoptName, SpecsDir: adoptDir, AnyCase: anyCase, MaxCost: *maxCost, RequestTimeout: *requestTimeout}
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
	// An agent's command has its own limit, ten minutes in Claude Code: a
	// first reading of 86 requirements ran past it, was killed, and the
	// agent waited nine minutes for word from a dead process (a live
	// session, 2026-11-10). Read for an agent, a check works for a slice
	// that fits, keeps what it did, and says to run it again.
	if *asJSON {
		// Asked again with nothing changed, the answer is the one kept: an
		// agent whose tool cut the JSON short ran the whole check three
		// more times to read it through `head` and `grep` (a live session,
		// 2026-11-10).
		if js, ok := sameAsBefore(*outDir, adoptName); ok {
			fmt.Fprintln(os.Stderr, "nothing changed since the last check: its result, as kept in "+filepath.Join(*outDir, "summary.json"))
			fmt.Fprintln(out, string(js))
			return 1
		}
	}
	sliced := *asJSON && !flagSet(fs, "timeout")
	if sliced {
		*timeout = slice
		if d, err := time.ParseDuration(os.Getenv("STELE_SLICE")); err == nil && d > 0 {
			*timeout = d // a harness with another limit, and the tests
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if sliced {
		// One spending limit over the whole check, across its slices.
		opt.SpendFile = filepath.Join(*outDir, "slice-spend")
	}
	res, err := change.Run(ctx, opt)
	if !(err != nil && sliced && ctx.Err() != nil) && opt.SpendFile != "" {
		_ = os.Remove(opt.SpendFile) // the check ended: the next one starts its own account
	}
	if err != nil && sliced && ctx.Err() != nil {
		read, total := readSoFar(*outDir), len(adoptStmts)
		progress := "The work so far is kept."
		if total > 0 && read > 0 {
			progress = fmt.Sprintf("%d of %d requirements are read so far, and the work is kept.", min(read, total), total)
		}
		js, _ := json.MarshalIndent(changeSummary{Change: id, Status: "continue", Questions: []change.Question{},
			Summary: "Stele is not done reading: a first reading takes more than one run. " + progress,
			Next:    "run the same command again now, as it was; it continues from what is kept. Tell the owner in one line that Stele is still reading and how far it is"}, "", "  ")
		_ = os.WriteFile(filepath.Join(*outDir, "summary.json"), append(js, '\n'), 0o644)
		fmt.Fprintln(out, string(js))
		return 1
	}
	if err == nil && len(res.Failed) > 0 && !slices.ContainsFunc(slices.Collect(maps.Values(res.Failed)), func(why string) bool { return !strings.HasPrefix(why, change.NoRules) }) {
		// The model answered with no rules for a few requirements. The next
		// check reads them once more and reports what stays so as unchecked;
		// run it here, so that a first adoption does not end, after every
		// other requirement is read, with "nothing is checked yet" and a
		// command to repeat (6 of 179 requirements, 2026-11-10). Everything
		// else is reused.
		fmt.Fprintf(os.Stderr, "reading %d requirement(s) the model gave no rules for once more ...\n", len(res.Failed))
		spent := res.Usage
		if opt.MaxCost > 0 {
			opt.MaxCost = max(opt.MaxCost-spent.CostUSD, 0.01)
		}
		if again, err2 := change.Run(ctx, opt); err2 == nil {
			again.Usage.Add(spent)
			res = again
		}
	}
	if err != nil {
		if *asJSON {
			// An agent reads the JSON: a run that stopped must still say so
			// there, not only on stderr.
			next := "the check stopped before it finished (see error); nothing new was checked. Fix what the error names, or raise --timeout, and run the same check again: translations already done are kept"
			if errors.Is(err, change.ErrChangedDuringCheck) {
				// --timeout has nothing to do with it (a trial, 2026-11-11).
				next = "a requirement or the registry was edited while the check ran, so its result was not kept. Run the same check again once the edit is done: translations already done are kept"
			}
			sum := changeSummary{Change: id, Status: "failed", Error: err.Error(), Questions: []change.Question{}, Next: next}
			if res != nil {
				sum.CostUSD = res.Usage.CostUSD
			}
			js, _ := json.MarshalIndent(sum, "", "  ")
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
	res.AdoptSpecs, res.UnreadSpecs = adoptSpecs, unreadSpecs
	js, _ := json.MarshalIndent(res, "", "  ")
	_ = os.WriteFile(filepath.Join(*outDir, "result.json"), append(js, '\n'), 0o644)
	if err := writeCard(res, head, summarize(res, *outDir).Status, *base); err != nil {
		fmt.Fprintf(os.Stderr, "stele change: could not write the PR card: %v\n", err)
	}
	blocked := len(res.Gate.Errors) > 0 || res.Gate.Check == nil || !res.Gate.Accepted || len(res.Failed) > 0
	if *asJSON {
		sum := summarize(res, *outDir)
		js, _ := json.MarshalIndent(sum, "", "  ")
		// The same JSON on disk: an adoption's is tens of kilobytes, more
		// than some agents' tools show of one command's output.
		_ = os.WriteFile(filepath.Join(*outDir, "summary.json"), append(js, '\n'), 0o644)
		if sum.Status == "questions" || sum.Status == "ready" {
			_ = os.WriteFile(filepath.Join(*outDir, "summary.key"), []byte(summaryKey(*outDir, res.Fingerprint, adoptName)), 0o644)
		} else {
			_ = os.Remove(filepath.Join(*outDir, "summary.key"))
		}
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
	Status string `json:"status"`
	Next   string `json:"next"`
	// Summary is the check in the owner's words: one short paragraph the
	// agent shows under the spec it drafted, so the owner reviews the spec
	// and Stele's reading in one act (the flow agreed on 2026-11-08).
	Summary   string            `json:"summary"`
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
	// Possible are contradictions a review of the texts found that the
	// rules do not show: the owner hears each before the approval.
	Possible []change.Possible `json:"possible_conflicts,omitempty"`
	// Specs are a large project's specs to choose from, with status
	// "choose": nothing was read yet.
	Specs []specChoice `json:"specs,omitempty"`
}

func summarize(res *change.Result, outDir string) changeSummary {
	s := changeSummary{Change: res.Change, Questions: res.Questions, Errors: res.Gate.Errors, Added: res.Added, Modified: res.Modified, Removed: res.Removed,
		Unchecked: res.Unchecked, Failed: res.Failed, NewTerms: res.NewTerms, Reused: len(res.Reused), CostUSD: res.Usage.CostUSD, Report: filepath.Join(outDir, "report.md")}
	if s.Questions == nil {
		s.Questions = []change.Question{}
	}
	// What an agent reads is kept short: a glossary repeated under every
	// requirement of every question, and the list of every new term of an
	// adoption, were a third of 36 kilobytes its tool cut short.
	for i := range s.Questions {
		reqs := slices.Clone(s.Questions[i].Requirements)
		for k := range reqs {
			reqs[k].Text, _, _ = strings.Cut(reqs[k].Text, "\nGlossary: ")
		}
		s.Questions[i].Requirements = reqs
	}
	if len(s.NewTerms) > 30 {
		s.NewTerms = append(slices.Clone(s.NewTerms[:30]), fmt.Sprintf("... and %d more, in the report", len(s.NewTerms)-30))
	}
	if len(s.Added) > 30 {
		s.Added = append(slices.Clone(s.Added[:30]), fmt.Sprintf("... and %d more", len(s.Added)-30))
	}
	for _, p := range res.Possible {
		p.Titles = nil
		for _, id := range p.Statements {
			// The spec too: two specs' "Requirement 5" read as one (2026-11-10).
			t := card.Title(res.Statements[id], id)
			if spec, label, ok := strings.Cut(id, "/"); ok {
				t = spec + ": " + t
				if label != "" && !strings.Contains(t, label) {
					t += " (" + label + ")"
				}
			}
			p.Titles = append(p.Titles, t)
		}
		s.Possible = append(s.Possible, p)
	}
	approve := len(res.Questions) == 1 && res.Questions[0].Kind == "approve"
	switch {
	case len(res.Gate.Errors) > 0:
		s.Status, s.Next = "broken", "the registry after this change does not compile; see errors"
	case len(res.Failed) > 0:
		// A reading Stele could not compile (type errors) arrives here as
		// well as a model call that failed; the next step said only the
		// latter, and the agent told the owner of a "missing role term"
		// (a live trial, 2026-11-08).
		s.Status, s.Next = "failed", fmt.Sprintf("Stele could not read %d requirement(s), so nothing was checked for them (see failed). If the error names the model command, its login or sandbox, fix that; otherwise Stele's reading did not compile, which is not the owner's to fix. Either way run the same check again, without rewording the spec: it retries them", len(res.Failed))
		if !slices.ContainsFunc(slices.Collect(maps.Values(res.Failed)), func(why string) bool { return !strings.HasPrefix(why, change.NoRules) }) {
			// The model answered, with no rules: nothing to fix but to ask again.
			s.Next = fmt.Sprintf("the model gave no rules for %d requirement(s) (see failed); run the same check again, without rewording them: it retries them once, and a requirement read so again is reported as unchecked", len(res.Failed))
		}
	case approve:
		// One act for the owner. With Stele's hook, Claude Code asks them
		// to allow the command that approves, so a chat question first
		// would ask twice; a clean approval took four of the owner's turns
		// (a review, 2026-11-09).
		run := fmt.Sprintf("`stele change answer %s %s approve`", res.Change, res.Questions[0].ID)
		s.Status, s.Next = "ready", "show the owner the spec and `summary`, and each of the approval question's `findings` in the requirement's words; say nothing more when there are none. Then ask the approval question, and record a yes with "+run
		if approvalPrompt(res.Repo) {
			s.Next = "show the owner the spec and `summary`, and each of the approval question's `findings` in the requirement's words; say nothing more when there are none. Then run " + run + ": Claude Code asks the owner to allow it, and their yes is the approval, so do not ask them in chat as well. If they deny it, ask what they want changed"
		}
		if n := notJudged(res); n > 0 {
			s.Next = fmt.Sprintf("Stele could not double-check its reading of %d requirement(s) in this run: run the same check once more first (it retries them); if it says this again, go on. Then: ", n) + s.Next
		}
	case len(res.Questions) > 0:
		s.Status, s.Next = "questions", "answer the questions with ask_owner false yourself; ask the owner the others in their words; record each with `stele change answer`, act on its result, then run `stele change` again"
		// Ten questions landed on an owner at once, with no word of how
		// many or what about (2026-11-07): say so before the first.
		if n := ownerQuestions(res.Questions); n > 1 {
			s.Next = fmt.Sprintf("answer the questions with ask_owner false yourself; %d questions wait for the owner: before the first, tell them in one sentence how many there are and what they are about, then ask them one at a time in their words; record each with `stele change answer`, act on its result, then run `stele change` again", n)
			if res.Change == change.Adoption {
				conflicts := 0
				for _, q := range res.Questions {
					if q.Kind == "conflict" && q.AskOwner {
						conflicts++
					}
				}
				if conflicts > 3 {
					s.Next += fmt.Sprintf(". %d are contradictions between requirements: tell the owner they may settle the ones that matter now and answer `later` to the rest, which stay recorded as open", conflicts)
				}
			}
		}
	case res.Gate.Accepted:
		s.Status, s.Next = "clean", "nothing to decide or approve"
	case res.Gate.Check != nil && len(res.Gate.Check.Undecided) > 0:
		// Not broken: the rules compile, and the solver could not settle
		// what one step must do (a developer trial reported it as broken,
		// with nothing to act on).
		s.Status = "undecided"
		s.Next = fmt.Sprintf("the solver could not decide what %s must do; answer the questions, then the approval says which rules it leaves unchecked. A simpler wording of the requirement behind that step may let Stele check it", strings.Join(res.Gate.Check.Undecided, ", "))
	default:
		s.Status, s.Next = "broken", "the check did not finish; see the report"
	}
	s.Summary = ownerSummary(res, s.Status)
	if res.Change == adoptChange {
		s.Next = forAdoption(s.Next)
		for i, q := range s.Questions {
			if q.Kind == "approve" {
				// An adoption is no change: the approval spoke of "this
				// change" to an owner adopting specs (a trial, 2026-11-10).
				t := strings.ReplaceAll(q.Text, "in this change", "in these specs")
				s.Questions[i].Text = strings.ReplaceAll(t, "Approve this change?", "Approve the adoption?")
			}
			opts := slices.Clone(q.Options)
			for k := range opts {
				opts[k].Then = forAdoption(opts[k].Then)
			}
			s.Questions[i].Options = opts
		}
	}
	return s
}

// ownerSummary is the check as the owner reads it: how many requirements,
// what they were checked against, whether they agree, what Stele cannot
// check, and what needs deciding. No rule syntax, no symbol names.
// ruleLegend tells the agent how to read Stele's rules before it explains
// them to the owner. An agent read count(p in instances(Packet): ...) <= 2
// as a limit of two, told the owner a correct reading of "at most three" was
// off by one, and spent two turns and a reread making it weaker (a live
// trial, 2026-11-08).
const ruleLegend = "How to read the rules: `instances(E)` is every other instance of E, never `self`, so `count(p in instances(Packet): ...) <= 2` on a packet allows three with that one; `old(x)` is x before the step; `result.x` is what the step reports. A rule that looks wrong is Stele's to settle: its findings below say where a reading is narrower or broader than the text. Explain the rules to the owner in the requirement's words, never their syntax, and do not reread a requirement only because a rule looks wrong to you."

func ownerSummary(res *change.Result, status string) string {
	title := func(id string) string { return `"` + card.Title(res.Statements[id], id) + `"` }
	n := len(res.Added) + len(res.Modified)
	// With no rule for any of them there is nothing that agrees or was
	// checked: the summary said a requirement agreed with the approved
	// ones and, in the next sentence, that Stele cannot check it (a
	// trial, 2026-11-09).
	noneChecked := n > 0 && len(res.Unchecked) == n
	approved := 0
	var b strings.Builder
	switch {
	case n == 0 && len(res.Removed) == 0:
		return "No requirement changed."
	case n == 0:
		fmt.Fprintf(&b, "This change retires %d requirement%s.", len(res.Removed), plural(len(res.Removed)))
	default:
		what := fmt.Sprintf("%d requirement%s", n, plural(n))
		if len(res.Added) > 0 && len(res.Modified) > 0 {
			what += fmt.Sprintf(" (%d new, %d edited)", len(res.Added), len(res.Modified))
		} else if len(res.Modified) > 0 {
			what += " (edited)"
		}
		fmt.Fprintf(&b, "Stele read %s", what)
		if reg, err := ir.LoadDir(res.Registry); err == nil {
			if approved = approvedStatements(reg); approved > 0 && !noneChecked {
				fmt.Fprintf(&b, " and checked %s against the %d approved before", pick(n == 1, "it", "them"), approved)
			}
		}
		b.WriteString(".")
		if len(res.Removed) > 0 {
			fmt.Fprintf(&b, " It retires %d.", len(res.Removed))
		}
	}
	switch status {
	case "failed":
		fmt.Fprintf(&b, " Stele could not read %d of them, so nothing is checked yet.", len(res.Failed))
		return b.String()
	case "broken":
		b.WriteString(" Stele could not check this change: its readings do not fit together.")
		return b.String()
	}
	var clashes, decisions []string
	for _, q := range res.Questions {
		if !q.AskOwner || q.Kind == "approve" {
			continue
		}
		var names []string
		for _, r := range q.Requirements {
			names = append(names, `"`+r.Title+`"`)
		}
		if q.Kind == "conflict" && len(names) == 2 {
			clashes = append(clashes, names[0]+" and "+names[1])
		} else if q.Kind == "conflict" && len(names) > 2 {
			// A clash of several is a contradiction too: the summary said
			// "2 other decisions" where the next step counted four
			// contradictions (a trial, 2026-11-10).
			clashes = append(clashes, fmt.Sprintf("%s and %d others", names[0], len(names)-1))
		} else {
			decisions = append(decisions, q.Kind)
		}
	}
	if len(clashes) == 0 && len(decisions) == 0 && !noneChecked {
		// With none approved yet, the check is the change against itself
		// (an adoption said "they agree with the approved requirements"
		// when there were none: a live trial, 2026-11-08).
		switch {
		case res.Open > 0:
			fmt.Fprintf(&b, " Apart from the %d contradiction%s decided later, they agree with each other.", res.Open, plural(res.Open))
		case res.SettledOpen+res.SettledPrecedence > 0:
			b.WriteString(" A reading of their texts shows no contradiction the rules prove.")
		case approved > 0:
			b.WriteString(pick(n == 1, " It agrees", " They agree") + " with the approved requirements.")
		case n > 1:
			b.WriteString(" They agree with each other; none was approved before.")
		default:
			b.WriteString(" Nothing contradicts it; none was approved before.")
		}
	}
	if len(clashes) > 0 {
		fmt.Fprintf(&b, " %s cannot both hold: %s.", pick(len(clashes) == 1, "Two requirements", "Some requirements"), strings.Join(clashes, "; "))
	}
	if len(decisions) > 0 {
		fmt.Fprintf(&b, " %d %sdecision%s %s needed.", len(decisions), pick(len(clashes) > 0, "other ", ""), plural(len(decisions)), pick(len(decisions) == 1, "is", "are"))
	}
	if len(res.Unchecked) > 0 {
		ids := slices.Sorted(maps.Keys(res.Unchecked))
		var names []string
		for _, id := range ids {
			names = append(names, title(id))
		}
		if len(ids) > 3 {
			// A first adoption named 59 of them, most by a heading several
			// share ("Requirement 4" seventeen times, 2026-11-10): the
			// count says it, and the report lists each with its text.
			fmt.Fprintf(&b, " Stele cannot check %d of them (requirements about the code or the process, judgments, or behaviour its rules cannot state) and would record them unchecked; the report lists each.", len(ids))
		} else {
			fmt.Fprintf(&b, " Stele cannot check %s and would record %s unchecked.", strings.Join(names, ", "), pick(len(ids) == 1, "it", "them"))
		}
	}
	// What the owner decides on is said; what a reading leaves out is a
	// count, with the clauses in the report.
	flagged, partial := 0, 0
	for _, id := range append(append([]string{}, res.Added...), res.Modified...) {
		if _, unchecked := res.Unchecked[id]; unchecked {
			continue // said above: nothing checks it, in part or whole
		}
		owner, notChecked, _ := change.SplitFindings(res.Findings[id])
		if len(owner) > 0 {
			flagged++
		}
		if len(notChecked) > 0 {
			partial++
		}
	}
	if partial > 0 {
		fmt.Fprintf(&b, " %d %s checked in part; the report lists the clauses no rule checks.", partial, pick(partial == 1, "is", "are"))
	}
	if n := res.SettledOpen + res.SettledPrecedence; n > 0 {
		fmt.Fprintf(&b, " Stele's rules clash in %d more place%s that a reading of the texts does not show as contradictions; it settled them itself (%d recorded as open, %d by taking the check or the error case first) and the report lists each.", n, plural(n), res.SettledOpen, res.SettledPrecedence)
	}
	if n := len(res.Possible); n > 0 {
		fmt.Fprintf(&b, " Read by their texts alone, %d more pair%s or group%s of requirements may contradict each other, which Stele's rules do not show (`possible_conflicts`): the owner should hear each before approving.", n, plural(n), plural(n))
	}
	if n := len(res.UnreadSpecs); n > 0 {
		fmt.Fprintf(&b, " %d other spec%s %s not read yet (%s): `stele adopt --spec <name>` reads one or more of them against what is approved.", n, plural(n), pick(n == 1, "is", "are"), strings.Join(res.UnreadSpecs, ", "))
	}
	if n := len(res.FlagOnly); n > 0 {
		fmt.Fprintf(&b, " %d %s recorded as a single yes/no fact nothing else reads (usually a requirement about how the code is built): Stele would catch only a later requirement that says the opposite.", n, pick(n == 1, "is", "are"))
	}
	if res.SetAside > 0 {
		fmt.Fprintf(&b, " Stele set aside %d of its own rules that clashed with another requirement and did not say what their requirement says; the report lists them.", res.SetAside)
	}
	if flagged > 0 {
		fmt.Fprintf(&b, " Its reading of %d says more than the text or blocks a step; the approval lists where.", flagged)
	}
	return b.String()
}

// notJudged counts the requirements whose reading Stele could not
// double-check in this run.
func notJudged(res *change.Result) int {
	n := 0
	for _, fs := range res.Findings {
		if _, _, k := change.SplitFindings(fs); k > 0 {
			n++
		}
	}
	return n
}

// approvalPrompt reports whether Claude Code asks the owner before an
// approval is recorded in this project: the PreToolUse entry `stele init`
// adds for Stele's hook, unless the session turned the prompt off.
func approvalPrompt(repo string) bool {
	if os.Getenv("STELE_APPROVAL_PROMPT") == "off" {
		return false
	}
	src, err := os.ReadFile(filepath.Join(repo, ".claude", "settings.json"))
	if err != nil {
		return false
	}
	var s struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	return json.Unmarshal(src, &s) == nil && steleHook(string(s.Hooks["PreToolUse"]))
}

// cmdChangeSummary prints the last check of a change as the owner reads
// it, in a line or two: what the permission prompt shows at the approval.
func cmdChangeSummary(args []string, out io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, "usage: stele change summary <change-id>\n")
		return 2
	}
	src, err := os.ReadFile(filepath.Join(".stele", "change", args[0], "result.json"))
	if err != nil {
		return failf("change summary", fmt.Errorf("no check for %s", args[0]))
	}
	var res change.Result
	if err := json.Unmarshal(src, &res); err != nil {
		return failf("change summary", err)
	}
	fmt.Fprintln(out, ownerSummary(&res, "ready"))
	return 0
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// ownerQuestions counts the questions for the owner.
func ownerQuestions(qs []change.Question) int {
	n := 0
	for _, q := range qs {
		if q.AskOwner {
			n++
		}
	}
	return n
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
		b.WriteString("## New terms\n\nStele's own names for what the code has, for the agent; they are not a decision for the owner.\n\n")
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
		b.WriteString("## Not translated\n\nStele could not read these requirements, so nothing was checked for them and the change cannot be approved yet. If an error names the model command, its login or sandbox, fix that; otherwise it is Stele's reading. Run the same check again; it retries them:\n\n")
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
		b.WriteString("## Rules to approve\n\nRead each rule as a reading of its requirement. The owner approves them by answering the check's approval question (`stele change answer <change> <approve question> approve`), which applies the change and records the approval.\n\n" + ruleLegend + "\n\n")
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
				fmt.Fprintf(&b, "- **%s**: %s\n  `%s`\n", id, head.Rule(id).Words(), ruleEnglish(head.Rule(id)))
			}
			for _, f := range res.Findings[s] {
				fmt.Fprintf(&b, "- ⚠ %s\n", oneLine(f, 300))
			}
			if cls := change.ClauseChecks(res.Clauses[s], head); len(cls) > 0 {
				b.WriteString("\nClause by clause:\n\n")
				for _, cl := range cls {
					if cl.Checked {
						fmt.Fprintf(&b, "- checked: %q, by: %s\n", cl.Text, strings.Join(cl.By, "; "))
					} else {
						fmt.Fprintf(&b, "- not checked: %q (%s)\n", cl.Text, cl.Why)
					}
				}
			}
			b.WriteString("\n")
		}
		if len(informal) > 0 {
			// Each with what it leaves unchecked: the IDs alone sent the
			// agent guessing which clause they were (a developer trial).
			b.WriteString("\nRecorded as informal or obligation, so reviewed rather than checked:\n\n")
			for _, id := range informal {
				if why := head.Rule(id).Rationale; why != "" {
					fmt.Fprintf(&b, "- %s: %s\n", id, oneLine(why, 300))
				} else {
					fmt.Fprintf(&b, "- %s\n", id)
				}
			}
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
	// Changes are the vocabulary changes that alter what its rules mean.
	Changes []string `json:"vocabulary_changes,omitempty"`
}

// ReviewRule is one rule to approve, with the reading it replaces.
type ReviewRule struct {
	ID      string `json:"id"`
	Reading string `json:"reading"`
	InWords string `json:"in_words"`
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
		r := ReviewRule{ID: id, Reading: ruleEnglish(x), InWords: x.Words()}
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

// copyDir makes to the tree from is: every file copied, and every file of
// to that from lacks removed. Approval applies the registry the change was
// checked as; a file the check dropped (writes no longer inferred, say)
// kept in the registry would make it a registry nobody checked.
func copyDir(from, to string) error {
	err := filepath.WalkDir(to, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(to, p)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(from, rel)); errors.Is(err, fs.ErrNotExist) {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return err
	}
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
	since := fs.String("since", "", "as --base, with the registry as a git ref holds it (HEAD: what the last commit approved)")
	all := fs.Bool("all", false, "with no --rules and no --base: approve every live rule not yet approved")
	by := fs.String("by", "", "who approves (the owner)")
	listOnly := fs.Bool("list", false, "list the rules that need approval, grouped by requirement with Stele's reading and what its checks found, and stop")
	fs.Bool("asked", false, "the owner was asked in chat and said yes: for a session where Claude Code cannot ask them to allow the command")
	asJSON := fs.Bool("json", false, "with --list: emit the review packet as JSON, for an agent to put to the owner")
	_ = fs.Parse(args)
	if *registry == "" || (*by == "" && !*listOnly) {
		fmt.Fprint(os.Stderr, "usage: stele approve --registry DIR (--rules A,B | --base DIR | --since REF | --all) --by NAME [--list]\n")
		return 2
	}
	reg, err := ir.LoadDir(*registry)
	if err != nil {
		return failf("approve", err)
	}
	var ids []string
	var baseReg *ir.Registry
	if *since != "" && *rules == "" {
		dir, cleanup, err := registryAt(*registry, *since)
		if err != nil {
			return failf("approve", err)
		}
		defer cleanup()
		*base = dir
	}
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
	var changes []gate.Change
	if baseReg != nil {
		for _, c := range gate.VocabChanges(baseReg, reg) {
			if len(c.Affected) > 0 {
				changes = append(changes, c)
			}
		}
	}
	if *listOnly {
		packet := reviewPacket(reg, baseReg, ids, *registry)
		for i := range packet {
			for _, c := range changes {
				for _, r := range packet[i].Rules {
					if slices.Contains(c.Affected, r.ID) {
						packet[i].Changes = append(packet[i].Changes, c.Words())
						break
					}
				}
			}
		}
		if !*asJSON {
			// What changed comes first: the readings alone did not say that
			// a value had gained a status (a trial, 2026-11-11).
			for _, c := range changes {
				fmt.Fprintf(out, "Vocabulary change: %s. It changes what %d approved rule(s) mean: %s\n", c.Words(), len(c.Affected), strings.Join(c.Affected, ", "))
			}
			if len(changes) > 0 {
				fmt.Fprintln(out)
			}
		}
		if *asJSON {
			js, _ := json.MarshalIndent(packet, "", "  ")
			fmt.Fprintln(out, string(js))
			return 0
		}
		for _, p := range packet {
			fmt.Fprintf(out, "%s\n  text: %s\n", p.Statement, oneLine(p.Text, 400))
			for _, r := range p.Rules {
				fmt.Fprintf(out, "  %s: %s\n    rule: %s\n", r.ID, r.InWords, r.Reading)
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
	if err := adopt.ApproveRulesUnder(reg, ids, *by, gate.MeaningChanges(baseReg, reg)); err != nil {
		return failf("approve", err)
	}
	fmt.Fprintf(out, "approved %d rule(s) as %s\n", len(ids), *by)
	for _, c := range changes {
		fmt.Fprintf(out, "  with the vocabulary change: %s\n", c.Words())
	}
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
	// A repository with no commit yet still names its branch.
	if b, err := exec.Command("git", "-C", repo, "symbolic-ref", "--short", "HEAD").Output(); err == nil {
		if name := strings.TrimSpace(string(b)); name != "" {
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

// specName is the spec a requirement belongs to: the part of its ID before
// the first slash, or "" when the project's requirements are one folder.
func specName(id string) string {
	name, _, ok := strings.Cut(id, "/")
	if !ok {
		return ""
	}
	return name
}

// partName names the registry files of specs adopted together.
func partName(specs []string) string {
	name := strings.Join(specs, "+")
	if len(name) > 60 {
		h := sha256.Sum256([]byte(name))
		name = fmt.Sprintf("%s+%d-more-%x", specs[0], len(specs)-1, h[:3])
	}
	return strings.Map(func(r rune) rune {
		if r == '+' || r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '-'
	}, name)
}

// readSpecsFile lists, in the registry, the specs adopted a part at a time.
const readSpecsFile = "adopted-specs.txt"

func readSpecs(registry string) []string {
	src, err := os.ReadFile(filepath.Join(registry, readSpecsFile))
	if err != nil {
		return nil
	}
	return strings.Fields(string(src))
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// largeAdoption is the number of requirements above which a first adoption
// is offered a part at a time.
const largeAdoption = 100

// specChoice is one spec of a large project, for the owner to choose from.
type specChoice struct {
	Name         string  `json:"name"`
	Requirements int     `json:"requirements"`
	Minutes      int     `json:"about_minutes"`
	Dollars      float64 `json:"about_usd"`
}

// estimate is what reading n requirements took on a held-out project: 179
// in about 13 minutes for $4.04, 46 in about 5 for $0.88 (2026-11-10).
func estimate(n int) (minutes int, dollars float64) {
	return max(2, (n*5+59)/60), float64(int(float64(n)*2.3+0.5)) / 100
}

// chooseSpecs answers a bare adoption of a large project with its specs and
// what reading each takes, and reads nothing.
func chooseSpecs(out io.Writer, asJSON bool, names []string, stmts []translate.Statement) int {
	var specs []specChoice
	for _, n := range names {
		count := 0
		for _, s := range stmts {
			if specName(s.ID) == n {
				count++
			}
		}
		m, d := estimate(count)
		specs = append(specs, specChoice{Name: n, Requirements: count, Minutes: m, Dollars: d})
	}
	m, d := estimate(len(stmts))
	summary := fmt.Sprintf("This project has %d requirements in %d specs. Stele has read none yet. Reading all of them at once takes about %d minutes and $%.2f; reading one or two specs first takes a few minutes, and the others can follow, each checked against what is approved.", len(stmts), len(names), m, d)
	next := "tell the owner the count and ask which specs to start with (the ones they are working on), offering each with its size from `specs`; then run `stele adopt --json --spec <name>[,<name>]`. If the owner wants everything now, run `stele adopt --json --all`, in the background or with a timeout of 30 minutes"
	if asJSON {
		js, _ := json.MarshalIndent(changeSummary{Change: adoptChange, Status: "choose", Summary: summary, Next: next, Questions: []change.Question{}, Specs: specs}, "", "  ")
		fmt.Fprintln(out, string(js))
		return 1
	}
	fmt.Fprintln(out, summary)
	for _, s := range specs {
		fmt.Fprintf(out, "  %s\t%d requirement(s)\tabout %d min, $%.2f\n", s.Name, s.Requirements, s.Minutes, s.Dollars)
	}
	fmt.Fprintln(out, "Next: `stele adopt --spec <name>[,<name>]`, or `stele adopt --all`.")
	return 1
}

// slice is how long one check works when an agent runs it, under the ten
// minutes Claude Code gives a command.
const slice = 8 * time.Minute

// readSoFar counts the requirements a check's translation has readings
// for, from the state its runs keep.
func readSoFar(outDir string) int {
	seen := map[string]bool{}
	states, _ := filepath.Glob(filepath.Join(outDir, "runs", "*", "formalize-state.json"))
	for _, f := range states {
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var st struct {
			Candidates []struct {
				StatementID string `json:"statement_id"`
			} `json:"candidates"`
		}
		if json.Unmarshal(src, &st) == nil {
			for _, c := range st.Candidates {
				seen[c.StatementID] = true
			}
		}
	}
	return len(seen)
}

// summaryKey identifies what a kept summary answers: the specs and the
// registry as fingerprinted, the answers recorded, and the part adopted.
func summaryKey(outDir, fingerprint, part string) string {
	answers, _ := os.ReadFile(filepath.Join(outDir, "answers.json"))
	h := sha256.Sum256(answers)
	return fmt.Sprintf("%s\n%x\n%s\n", fingerprint, h[:8], part)
}

// sameAsBefore returns the summary the last check kept, when the specs, the
// registry and the answers are what that check saw.
func sameAsBefore(outDir, part string) ([]byte, bool) {
	key, err := os.ReadFile(filepath.Join(outDir, "summary.key"))
	if err != nil {
		return nil, false
	}
	src, err := os.ReadFile(filepath.Join(outDir, "result.json"))
	if err != nil {
		return nil, false
	}
	var res change.Result
	if json.Unmarshal(src, &res) != nil || len(res.Watched) == 0 {
		return nil, false
	}
	now, err := change.Fingerprint(res.Watched...)
	if err != nil || summaryKey(outDir, now, part) != string(key) {
		return nil, false
	}
	js, err := os.ReadFile(filepath.Join(outDir, "summary.json"))
	if err != nil {
		return nil, false
	}
	return bytes.TrimRight(js, "\n"), true
}

// registryAt copies the registry at dir as the git ref holds it into a
// temporary folder.
func registryAt(dir, ref string) (string, func(), error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	top := exec.Command("git", "rev-parse", "--show-toplevel")
	top.Dir = abs
	b, err := top.Output()
	if err != nil {
		return "", nil, fmt.Errorf("--since needs the registry in a git repository: %w", err)
	}
	root := strings.TrimSpace(string(b))
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if a, err := filepath.EvalSymlinks(abs); err == nil {
		abs = a
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", nil, err
	}
	rel = filepath.ToSlash(rel)
	tmp, err := os.MkdirTemp("", "stele-since-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	archive := exec.Command("git", "archive", "--format=tar", ref, "--", rel)
	archive.Dir = root
	tarball, err := archive.Output()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("%s holds no registry at %s", ref, rel)
	}
	untar := exec.Command("tar", "-x", "-C", tmp)
	untar.Stdin = bytes.NewReader(tarball)
	if out, err := untar.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("tar: %v: %s", err, out)
	}
	return filepath.Join(tmp, rel), cleanup, nil
}
