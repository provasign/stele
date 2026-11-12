// Package cli implements the stele command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/provasign/stele/internal/graph"
	"github.com/provasign/stele/internal/lint"
	"github.com/provasign/stele/internal/version"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// usage is what a person who installed Stele needs: the commands the
// owner, the agent and CI run in a project. The research and pipeline
// commands are under `stele help all`; forty commands in one list hid the
// six that matter (a review, 2026-11-09).
const usage = `usage: stele <command> [flags]

Stele checks every spec change against the requirements the owner approved.
Most of it runs through your coding agent: run "stele init", then work as usual.

set up
  init [dir]             create stele.yaml and the registry, and set up Claude Code (the one agent supported for now)
  version [--solver]     print the release; --solver also checks the Z3 it runs

check a change (the agent runs these; the owner answers its questions)
  adopt [repo]           check a project's existing specs once: every requirement against every other
  change openspec <repo> <id>            check one OpenSpec change folder against the approved requirements
  change speckit|kiro <repo> --base REF  check a feature branch's spec changes
  change markdown <repo>                 check the requirements a project keeps in its own folder
  change shale <repo> --base REF         check the requests kept as requirements
  change answer <id> <q> <choice>        record an answer to a check's question; approve applies the change
  keep [request]         keep a request declared with shale intent as a requirement; --list shows every request's state
  lesson add|list|remove what the owner taught Stele about this project

keep it consistent
  check [registry-dir]   every approved requirement against every other (run after a merge or pull)
  gate --base DIR --head DIR   decide whether a registry change may merge (CI)
  sync                   compare the specs with the approved requirements; --apply records moved ones
  rules [request]        the approved requirements a request touches
  touches                the approved requirements the current code change touches

"stele help all" lists every command; "stele <command> -h" its flags.
`

const usageAll = `usage: stele <command> [flags]

commands:
  init [dir]             create stele.yaml and an empty registry, and set up Claude Code, the one coding agent supported for now
  check [registry-dir]   find rules that no system response can satisfy together; in a project with no
                         directory, every approved requirement against every other (run after a merge or pull)
  gate --base DIR --head DIR   decide whether a registry change may merge (CI)
  formalize              build a whole registry from spec statements: vocabulary, translation, gaps closed
  refine                 review the rules in conflicts against their own statements; translate the unfaithful ones again
  bridge                 add the world facts that connect the statements of suspected contradictions
  normalize              find attributes that name one fact twice across the corpus, and how facts relate
  verify                 judge each conflict as real, resolved in the text, uncertain or a translation artifact
  translate              translate spec statements into candidate IR with an LLM
  answer                 choose a reading for an ambiguous translated statement
  vocab propose          propose vocabulary from spec statements (for projects without schemas or API definitions)
  score                  compare a translated registry with a hand-written reference
  bench                  score saved formalize runs on benchmark sets: coverage, known conflicts, time
  import speckit|openspec|markdown|shale <dir>  extract normative statements from a Spec Kit or OpenSpec repository, any markdown, or the requests agents declared with Shale
  review                 LLM-only baseline: ask a model for contradictions directly
  recall                 score translate+check and review against known conflicts
  adopt [repo]           check a project's existing specs once: every requirement against every other, questions for the owner
  change openspec <repo> <id>  check one OpenSpec change folder against the approved registry before applying it
  change speckit|kiro|shale <repo> --base REF  check a feature branch's spec changes, or its Shale requests, against the approved registry
  change markdown <repo>  check the requirements a project keeps in its own folder against the approved registry
  change answer <id> <q> <choice>  record the owner's answer to a change check's question; approve applies the change
  approve                record the owner's approval of rules, pinned to their statements' text
  lesson add|list|remove what the owner taught Stele about this project (most come from change answers on their own)
  audit <repo> --out DIR  the whole review-first pipeline: import, hygiene, review, adjudicate, report.md
  adjudicate             decide review candidates from the code: which statements it follows, newer or older
  hygiene                structural pass: restated titles, misplaced spec folders, code names the code lacks
  candidates             the statements a review cites, to model them alone (adoption, docs/08 W5)
  confirm                mark each review finding confirmed, not reproduced or unproved by the checker
  triage                 group the findings and draft each decision from git dates, for the owners
  baseline               write the decided triage as decisions and open conflicts, so the gate passes
  writeback              note each superseded statement in the spec files
  sync                   compare a fresh import with the registry's statements; retire removed ones
  watch                  synchronize saved-spec diagnostics and pending work (CLI, hooks, CI)
  lsp                    language server for saved-spec sync diagnostics in IDEs
  serve                  keep registries, solver and results in memory; stele check uses it when it runs
  keep [request]         keep a request declared with shale intent as a requirement (default: the newest); --drop makes it a task again; --decline sets it aside for good; --list shows every request's state
  export openspec        write the kept requests as the project's first OpenSpec spec; the approved rules follow them
  rules [request]        the approved requirements a request touches (stdin or arguments), for planning
  touches                the approved requirements the current code change touches (the branch and working tree)
  hook                   the coding agent's hook stele init installs (Claude Code; --agent codex)
  precommit              the git pre-commit check stele init installs: blocks specs and approved rules that disagree
  version [--solver]     print the release; --solver also checks the Z3 it runs
`

// Run executes the command line and returns the process exit code:
// 0 pass, 1 conflicts found, 2 usage or load error, 3 unknown (solver timeout).
func Run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "init":
		return cmdInit(args[1:], os.Stdin, os.Stdout, stdinIsTerminal())
	case "check":
		if code, ok := viaServer(args); ok {
			return code
		}
		return cmdCheck(args[1:], os.Stdout)
	case "serve":
		return cmdServe(args[1:], os.Stderr)
	case "rules":
		return cmdRules(args[1:], os.Stdin, os.Stdout)
	case "touches":
		return cmdTouches(args[1:], os.Stdout)
	case "hook":
		return cmdHook(args[1:], os.Stdin, os.Stdout)
	case "adopt":
		return cmdChange(append([]string{adoptChange}, defaultRepo(args[1:])...), os.Stdout)
	case "precommit":
		return cmdPrecommit(args[1:], os.Stderr)
	case "version", "--version":
		fmt.Fprintln(os.Stdout, "stele "+version.Version)
		if slices.Contains(args[1:], "--solver") {
			return solverCheck(os.Stdout, os.Stderr)
		}
		return 0
	case "gate":
		return cmdGate(args[1:], os.Stdout)
	case "translate":
		return cmdTranslate(args[1:], os.Stdout)
	case "formalize":
		return cmdFormalize(args[1:], os.Stdout)
	case "refine":
		return cmdRefine(args[1:], os.Stdout)
	case "verify":
		return cmdVerify(args[1:], os.Stdout)
	case "normalize":
		return cmdNormalize(args[1:], os.Stdout)
	case "bridge":
		return cmdBridge(args[1:], os.Stdout)
	case "vocab":
		return cmdVocab(args[1:], os.Stdout)
	case "answer":
		return cmdAnswer(args[1:], os.Stdout)
	case "score":
		return cmdScore(args[1:], os.Stdout)
	case "bench":
		return cmdBench(args[1:], os.Stdout)
	case "import":
		return cmdImport(args[1:], os.Stdout)
	case "review":
		return cmdReview(args[1:], os.Stdout)
	case "recall":
		return cmdRecall(args[1:], os.Stdout)
	case "change":
		return cmdChange(args[1:], os.Stdout)
	case "keep":
		return cmdKeep(args[1:], os.Stdout)
	case "export":
		return cmdExport(args[1:], os.Stdout)
	case "approve":
		return cmdApprove(args[1:], os.Stdout)
	case "lesson":
		return cmdLesson(args[1:], os.Stdout)
	case "audit":
		return cmdAudit(args[1:], os.Stdout)
	case "adjudicate":
		return cmdAdjudicate(args[1:], os.Stdout)
	case "hygiene":
		return cmdHygiene(args[1:], os.Stdout)
	case "candidates":
		return cmdCandidates(args[1:], os.Stdout)
	case "confirm":
		return cmdConfirm(args[1:], os.Stdout)
	case "triage":
		return cmdTriage(args[1:], os.Stdout)
	case "baseline":
		return cmdBaseline(args[1:], os.Stdout)
	case "writeback":
		return cmdWriteback(args[1:], os.Stdout)
	case "sync":
		return cmdSync(args[1:], os.Stdout)
	case "watch":
		return cmdWatch(args[1:], os.Stdout)
	case "lsp":
		return cmdLSP(args[1:], os.Stdin, os.Stdout)
	case "help", "-h", "--help":
		if len(args) > 1 && args[1] == "all" {
			fmt.Fprint(os.Stdout, usageAll)
			return 0
		}
		fmt.Fprint(os.Stdout, usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "stele: unknown command %q\n%s", args[0], usage)
	return 2
}

func cmdCheck(args []string, out io.Writer) int { return checkCmd(args, out, os.Stderr) }

// checkCmd is `stele check`, writing errors to errOut, so that `stele serve`
// can answer it for a client.
func checkCmd(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(errOut)
	jsonOut := fs.Bool("json", false, "emit JSON")
	ignoreDecisions := fs.Bool("ignore-decisions", false, "check the rules as written, without decision records")
	timeout := fs.Duration("timeout", 10*time.Second, "per-query solver budget; a timeout is reported as unknown")
	assumeDefaults := fs.Bool("assume-defaults", false, "check only situations where every configured default holds (the shipped configuration)")
	z3Path := fs.String("z3", "", "path to a z3 binary to run (default: Stele's own Z3, else z3 on PATH)")
	cachePath := fs.String("cache", "", "file of per-unit results to reuse and update: units whose questions are unchanged are not solved again")
	groups := fs.Bool("groups", false, "list conflicts grouped by the statement they share, as questions for the specs' authors")
	// Accept `stele check <dir> --flags`: flag parsing stops at the first positional.
	dir := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if dir == "" {
		// In a project: its registry, reported as requirements.
		cfg, err := config.Find(".")
		if err != nil {
			fmt.Fprintf(errOut, "stele check: %v\n", err)
			return 2
		}
		if cfg != nil {
			return checkProject(cfg, *jsonOut, *z3Path, *timeout, out, errOut)
		}
		fmt.Fprint(errOut, "usage: stele check [<registry-dir>] [--json] [--ignore-decisions]\n")
		return 2
	}

	reg, prog, err := loadProgram(dir, lower.Options{IgnoreDecisions: *ignoreDecisions, AssumeDefaults: *assumeDefaults})
	if err != nil {
		fmt.Fprintf(errOut, "stele check: %v\n", err)
		return 2
	}
	opt := checker.Options{Cache: checker.DefaultCache()}
	if !*ignoreDecisions {
		opt = gate.CheckOptions(reg)
	}
	if *cachePath != "" {
		opt.Cache = &checker.Cache{}
		if src, err := os.ReadFile(*cachePath); err == nil {
			if err := json.Unmarshal(src, opt.Cache); err != nil {
				fmt.Fprintf(errOut, "stele check: %s: %v (starting an empty cache)\n", *cachePath, err)
				opt.Cache = &checker.Cache{}
			}
		}
	}
	res, err := checker.CheckWith(context.Background(), &solver.Z3{Path: *z3Path, Timeout: *timeout}, prog, opt)
	if err != nil {
		fmt.Fprintf(errOut, "stele check: %v\n", err)
		return 2
	}
	if *cachePath != "" {
		if js, err := json.Marshal(opt.Cache); err == nil {
			if err := os.WriteFile(*cachePath, js, 0o644); err != nil {
				fmt.Fprintf(errOut, "stele check: %v\n", err)
			}
		}
		fmt.Fprintf(errOut, "cache: %d unit(s) reused, %d checked\n", opt.Cache.Hits, opt.Cache.Misses)
	}
	findings := lint.Rules(reg)
	suspect := suspects(reg, res, findings)
	if *groups && !*jsonOut {
		printGroups(out, reg, res, findings, suspect)
		switch res.Verdict {
		case checker.Fail:
			return 1
		case checker.Unknown:
			return 3
		}
		return 0
	}
	// A rule that fixes a value makes the value's others impossible, which
	// is what it says: the gate sets these aside, and so does the check,
	// or a registry the gate adopted failed its first check (a first
	// adoption, 2026-11-10).
	pinned := gate.Pinned(res, reg)
	var open []gate.OpenStatus
	if !*ignoreDecisions {
		open = gate.Ratchet(res, reg)
	} else if res.Verdict == checker.Fail && len(res.Conflicts) == 0 && len(res.Dead) == 0 {
		res.Verdict = checker.Pass
		if len(res.Undecided) > 0 {
			res.Verdict = checker.Unknown
		}
	}

	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			*checker.Result
			Open     []gate.OpenStatus `json:"open_conflicts,omitempty"`
			Retired  []string          `json:"retired,omitempty"`
			Skipped  []string          `json:"skipped,omitempty"`
			Warnings []string          `json:"warnings,omitempty"`
			Lint     []lint.Finding    `json:"lint,omitempty"`
			// Suspect maps a conflict's index to why it is likely a
			// translation error rather than a disagreement of the specs.
			Suspect map[int]string         `json:"suspect,omitempty"`
			Pinned  []checker.DeadResponse `json:"pinned_values,omitempty"`
		}{res, open, prog.Retired, prog.Skipped, prog.Warnings, findings, suspect, pinned})
	} else {
		printText(out, dir, reg, prog, res, open, *ignoreDecisions)
		for _, d := range pinned {
			fmt.Fprintf(out, "  fixed by %s: %s never happens, as the rule says\n", strings.Join(d.Rules, ", "), d.Response)
		}
	}
	switch res.Verdict {
	case checker.Fail:
		return 1
	case checker.Unknown:
		return 3
	}
	return 0
}

func printText(out io.Writer, dir string, reg *ir.Registry, prog *lower.Program, res *checker.Result, open []gate.OpenStatus, ignoreDecisions bool) {
	mode := fmt.Sprintf("%d decision(s) applied", len(reg.Decisions))
	if ignoreDecisions {
		mode = "rules as written, decisions and open conflicts ignored"
	}
	fmt.Fprintf(out, "stele check %s: %d rule(s) over %d operation(s), %s\n", dir, prog.RuleCount(), len(prog.Units)-1, mode)
	for _, a := range prog.Assumed() {
		fmt.Fprintf(out, "  assuming %s (%s)\n", a.Rule.ID, strings.TrimPrefix(a.Rule.Source, "specs/"))
	}
	for _, s := range prog.Retired {
		fmt.Fprintf(out, "  %s\n", s)
	}
	for _, w := range prog.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
	fmt.Fprintln(out)

	notes := prog.Notes()
	rules := func(ids []string) {
		for _, id := range ids {
			r := reg.Rule(id)
			if r == nil { // an operation's declared effect
				fmt.Fprintf(out, "         - %s  [the operation's effect]\n", id)
				continue
			}
			fmt.Fprintf(out, "         - %s  [%s]\n", id, r.Source)
			if r.Prose != "" {
				fmt.Fprintf(out, "           %q\n", r.Prose)
			}
			if n := notes[id]; n != "" {
				fmt.Fprintf(out, "           (%s)\n", n)
			}
		}
	}
	for i, c := range res.Conflicts {
		where := "no state satisfies all of these"
		switch c.Unit {
		case "state":
		case checker.AssumptionsUnit:
			where = "these requirements on the environment cannot all hold"
		default:
			where = "no outcome of " + c.Unit + " satisfies all of these"
		}
		fmt.Fprintf(out, "[FAIL] conflict %d: %s\n", i+1, strings.Join(c.Rules, ", "))
		fmt.Fprintf(out, "       counterexample: %s\n", strings.Join(situation(reg, c.Counterexample), ", "))
		fmt.Fprintf(out, "       %s:\n", where)
		rules(c.Rules)
	}
	neverAllowed := 0
	for _, d := range res.Dead {
		if d.NeverAllowed {
			neverAllowed++
			continue
		}
		fmt.Fprintf(out, "[FAIL] dead: %s can never happen\n", d.Response)
		if len(d.Rules) == 0 {
			fmt.Fprintln(out, "       no rule is involved: the declared ranges, the operation's default, or the invariants and assumptions about the state before rule it out")
			continue
		}
		fmt.Fprintln(out, "       these rules rule it out in every situation:")
		rules(d.Rules)
	}
	for _, d := range res.Dead {
		if d.NeverAllowed {
			fmt.Fprintf(out, "[FAIL] never allowed: %s never runs: its default is deny and no rule allows it (set default: allow, or add the rule that allows it)\n", d.Unit)
		}
	}
	for _, v := range res.Vacuous {
		if v.Avoided {
			fmt.Fprintf(out, "[warn] vacuous: %s holds only if its situation never arises; these rules forbid what it requires there:\n", v.Rule)
			rules(v.Because)
			continue
		}
		fmt.Fprintf(out, "[warn] vacuous: %s never applies; its condition can never hold\n", v.Rule)
		if len(v.Because) == 0 {
			fmt.Fprintln(out, "       the condition contradicts itself")
			continue
		}
		fmt.Fprintln(out, "       these rules exclude it:")
		rules(v.Because)
	}
	for _, c := range res.Unreachable {
		fmt.Fprintf(out, "[note] unreachable: %s clash only in a state no sequence of steps enters: %s\n", strings.Join(c.Rules, ", "), strings.Join(situation(reg, c.Counterexample), ", "))
	}
	printOpen(out, open)
	if len(res.Undecided) > 0 {
		fmt.Fprintf(out, "[unknown] the solver gave up on %d unit(s), which are not checked: %s\n", len(res.Undecided), strings.Join(res.Undecided, ", "))
	}
	if len(res.NearLimit) > 0 {
		fmt.Fprintf(out, "[warning] decided within 20%% of the solver timeout, so a slower machine would leave them undecided: %s\n", strings.Join(res.NearLimit, ", "))
	}
	switch res.Verdict {
	case checker.Pass:
		fmt.Fprintf(out, "[pass] no conflicts (%s)\n", res.Solver)
	case checker.Unknown:
		fmt.Fprintf(out, "[unknown] solver gave up; this is not a pass (%s)\n", res.Solver)
	default:
		fmt.Fprintf(out, "\n%d conflict(s), %d dead, %d never allowed (%s)\n", len(res.Conflicts), len(res.Dead)-neverAllowed, neverAllowed, res.Solver)
	}
}

// printGroups lists the conflicts as questions for the specs' authors: one
// group per statement most of them share, with the statements it conflicts
// with and their text, so the author reads root causes, not combinations.
// suspects marks the conflicts that are likely translation errors: all
// their rules come from one statement, or one of their rules is linted.
func suspects(reg *ir.Registry, res *checker.Result, findings []lint.Finding) map[int]string {
	out := map[int]string{}
	flagged := map[string]string{}
	for _, f := range findings {
		flagged[f.Rule] = f.Kind
	}
	var sets [][]string
	for _, c := range res.Conflicts {
		sets = append(sets, c.Rules)
	}
	for _, i := range lint.SelfConflicts(reg, sets) {
		out[i] = "the rules of one statement leave no outcome; the translation may miss a fact the statement presumes (such as names being unique) or assert more than it says"
	}
	for i, c := range res.Conflicts {
		for _, id := range c.Rules {
			if k := flagged[id]; k != "" && out[i] == "" {
				out[i] = k + " (" + id + ")"
			}
		}
	}
	return out
}

func printGroups(out io.Writer, reg *ir.Registry, res *checker.Result, findings []lint.Finding, suspect map[int]string) {
	stmtOf := func(id string) string {
		if r := reg.Rule(id); r != nil {
			return r.Statement()
		}
		return id // an operation's own constraint
	}
	prose, source := map[string]string{}, map[string]string{}
	for _, r := range reg.Rules {
		s := r.Statement()
		if prose[s] == "" {
			prose[s], source[s] = r.Prose, r.Source
		}
	}
	var sets [][]string
	var index []int // conflict index of each set
	for i, c := range res.Conflicts {
		if suspect[i] != "" {
			continue
		}
		index = append(index, i)
		var ss []string
		for _, id := range c.Rules {
			if s := stmtOf(id); !slices.Contains(ss, s) {
				ss = append(ss, s)
			}
		}
		sets = append(sets, ss)
	}
	gs := graph.Groups(sets)
	fmt.Fprintf(out, "%d conflict(s): %d in %d group(s), each one question for the specs' authors; %d likely translation error(s)\n", len(res.Conflicts), len(sets), len(gs), len(suspect))
	show := func(s string) {
		text := strings.Join(strings.Fields(prose[s]), " ")
		if len(text) > 200 {
			text = text[:200] + "…"
		}
		fmt.Fprintf(out, "    %s  [%s]\n      %q\n", s, source[s], text)
	}
	for i, g := range gs {
		fmt.Fprintf(out, "\nG-%d: %d conflict(s)\n", i+1, len(g.Conflicts))
		if g.Root != "" {
			fmt.Fprintf(out, "  this statement\n")
			show(g.Root)
			fmt.Fprintf(out, "  cannot hold together with\n")
		}
		for _, s := range g.Others {
			show(s)
		}
		c := res.Conflicts[index[g.Conflicts[0]]]
		if sit := situation(reg, c.Counterexample); len(sit) > 0 {
			fmt.Fprintf(out, "  for example when: %s\n", strings.Join(sit, ", "))
		}
	}
	if len(suspect) > 0 || len(findings) > 0 {
		fmt.Fprintf(out, "\nlikely translation errors (fix the rules, not the specs):\n")
		for i, c := range res.Conflicts {
			if why := suspect[i]; why != "" {
				fmt.Fprintf(out, "  conflict of %s: %s\n", strings.Join(c.Rules, ", "), why)
			}
		}
		for _, f := range findings {
			fmt.Fprintf(out, "  rule %s: %s\n", f.Rule, f.Message)
		}
	}
	if len(res.Undecided) > 0 {
		fmt.Fprintf(out, "\nundecided (the solver gave up; not a pass): %s\n", strings.Join(res.Undecided, ", "))
	}
	if len(res.NearLimit) > 0 {
		fmt.Fprintf(out, "\nnear the solver timeout (decided, but a slower machine would not): %s\n", strings.Join(res.NearLimit, ", "))
	}
}

// defaultRepo puts "." first when no repository is named.
func defaultRepo(args []string) []string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return append([]string{"."}, args...)
	}
	return args
}

// solverCheck runs one small script through the solver Stele uses, and says
// which it is: the Z3 built into this binary, or a z3 binary.
func solverCheck(out, errOut io.Writer) int {
	z := &solver.Z3{Timeout: 5 * time.Second}
	v, err := z.Version()
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	res, err := z.Run(context.Background(), "(declare-const x Int)\n(assert (> x 2))\n(check-sat)\n")
	if err != nil || res.Status != solver.Sat {
		fmt.Fprintf(errOut, "%s gave no correct answer: %v\n", v, err)
		return 1
	}
	where := "z3 on PATH"
	if p := os.Getenv("STELE_Z3"); p != "" {
		where = p
	} else if solver.Builtin() {
		where = "built in"
	}
	fmt.Fprintf(out, "%s (%s)\n", v, where)
	return 0
}
