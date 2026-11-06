package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

func cmdImport(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	outPath := fs.String("out", "", "statements file to write")
	dates := fs.Bool("dates", false, "openspec only: record when each statement's text was written (git blame)")
	anyCase := fs.Bool("any-case", false, "markdown only: also take lower-case must, shall, should and required")
	if len(args) < 2 || (args[0] != "speckit" && args[0] != "openspec" && args[0] != "kiro" && args[0] != "markdown" && args[0] != "shale") {
		fmt.Fprint(os.Stderr, "usage: stele import speckit|openspec|kiro|markdown|shale <dir> --out statements.yaml [--dates] [--any-case]\n")
		return 2
	}
	format, repo := args[0], args[1]
	_ = fs.Parse(args[2:])
	var stmts []translate.Statement
	var err error
	switch format {
	case "openspec":
		stmts, err = importer.OpenSpec(repo, *dates)
	case "kiro":
		stmts, err = importer.Kiro(repo)
	case "markdown":
		stmts, err = importer.Markdown(repo, importer.MarkdownOptions{AnyCase: *anyCase})
	case "shale":
		stmts, err = importer.Shale(repo)
	default:
		stmts, err = importer.SpecKit(repo)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele import: %v\n", err)
		return 2
	}
	src, err := yaml.Marshal(stmts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele import: %v\n", err)
		return 2
	}
	header := fmt.Sprintf("# %d statements extracted by `stele import %s %s`.\n", len(stmts), format, repo)
	if *outPath == "" {
		_, _ = out.Write(append([]byte(header), src...))
		return 0
	}
	if err := os.WriteFile(*outPath, append([]byte(header), src...), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "stele import: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "wrote %d statements to %s\n", len(stmts), *outPath)
	return 0
}

// llmFlags registers the backend flags shared by translate and review, with
// the translation models and efforts as defaults.
func llmFlags(fs *flag.FlagSet) func() (llm.Client, error) {
	return llmFlagsDefaults(fs, translationModel, translationEffort)
}

// translationModel is the model translation asks a backend for when neither
// --model nor stele.yaml gives one. On the claude CLI it is Sonnet: on a
// 60-requirement held-out set it read the whole set, rereads included, for
// $1.34 in 5 minutes, where Opus cost $5.03 and took 21 minutes of model
// time, and the Opus judge scored Sonnet's rules at 111 clauses covered to
// 55 uncovered against Opus's 122 to 26 (2026-11-06); the owner put cost and
// speed first. Codex keeps the user's own default model.
var translationModel = map[string]string{"claude": "claude-sonnet-5-5"}

// translationEffort is the reasoning effort translation asks a backend for
// when neither --effort nor stele.yaml gives one. Codex otherwise runs at
// the user's own default: at high, a six-requirement Spec Kit change took 7
// to 20 minutes, its calls stalling past the request limit and retried
// whole, and at medium 3 to 5 minutes with none stalled and the readings
// as good (a developer trial, 2026-11-05).
var translationEffort = map[string]string{"codex": "medium"}

// llmFlagsEffort is llmFlags with a reasoning effort per backend when
// --effort is not given (the anthropic backend has its own default), and
// the backend's own default model.
func llmFlagsEffort(fs *flag.FlagSet, effortDefault map[string]string) func() (llm.Client, error) {
	return llmFlagsDefaults(fs, nil, effortDefault)
}

// llmFlagsDefaults is llmFlags with a model and a reasoning effort per
// backend, used when --model and --effort are not given.
func llmFlagsDefaults(fs *flag.FlagSet, modelDefault, effortDefault map[string]string) func() (llm.Client, error) {
	backend := fs.String("llm", "anthropic", "LLM backend: anthropic, openai (API keys), claude, codex (local CLIs), cmd")
	model := fs.String("model", "", "model for the backend (anthropic defaults to claude-opus-5-5, the claude CLI to claude-sonnet-5-5 for translation; codex uses its own default)")
	effort := fs.String("effort", "", "reasoning effort: low, medium, high, xhigh, max (anthropic, claude); codex takes its own levels, e.g. minimal, low, medium, high")
	command := fs.String("llm-cmd", "", "for --llm cmd: shell command that reads the prompt on stdin and prints the reply")
	return func() (llm.Client, error) {
		if *command != "" && !flagSet(fs, "llm") {
			*backend = "cmd" // --llm-cmd alone selects the command backend
		}
		if *effort == "" {
			*effort = effortDefault[*backend]
		}
		if *model == "" {
			*model = modelDefault[*backend]
		}
		return llm.New(llm.Config{Backend: *backend, Model: *model, Effort: *effort, Command: *command})
	}
}

func cmdReview(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file")
	vocab := fs.String("world-facts", "", "comma-separated YAML files whose rules are given to the reviewer as world facts")
	outPath := fs.String("out", "", "review JSON to write")
	timeout := fs.Duration("timeout", 60*time.Minute, "limit for the run")
	budget := fs.Int("budget-tokens", 150000, "split a corpus larger than this into area reviews plus a condensed review of all statements (0: one prompt)")
	parallel := fs.Int("parallel", 2, "with --budget-tokens: review calls at a time")
	planOnly := fs.Bool("plan", false, "print the review calls and their estimated size, and stop")
	// High effort, as on the anthropic backend: on Project A 001-015 the
	// review found 3 to 5 of 10 known conflicts at the CLI's default effort
	// and 6 to 9 at high, for no more cost (stele-research research/bench/sweep, step 7).
	client := llmFlagsEffort(fs, map[string]string{"claude": "high"})
	_ = fs.Parse(args)
	if *stmtsPath == "" || (*outPath == "" && !*planOnly) {
		fmt.Fprint(os.Stderr, "usage: stele review --statements FILE --out review.json [--budget-tokens N] [--plan] [--world-facts domain.yaml] [--llm ...]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele review: %v\n", err)
		return 2
	}
	batches, warning := review.Plan(stmts, *budget)
	if warning != "" {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
	if *planOnly || len(batches) > 1 {
		total := 0
		for _, b := range batches {
			total += b.Tokens
			fmt.Fprintf(out, "  %-5s ~%6d tokens  %s\n", b.Kind, b.Tokens, b.Label)
		}
		fmt.Fprintf(out, "%d review call(s), ~%d prompt tokens in all\n", len(batches), total)
		if *planOnly {
			return 0
		}
	}
	var facts []review.Fact
	if *vocab != "" {
		reg, err := ir.LoadFiles(strings.Split(*vocab, ",")...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stele review: %v\n", err)
			return 2
		}
		for _, r := range reg.Rules {
			if r.Prose != "" {
				facts = append(facts, review.Fact{ID: r.ID, Text: r.Prose})
			}
		}
	}
	m, err := client()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele review: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	fmt.Fprintf(out, "reviewing %d statement(s) with %s ...\n", len(stmts), m.Name())
	var res *review.Result
	if len(batches) == 1 {
		if res, err = review.Review(ctx, m, stmts, facts); err != nil {
			fmt.Fprintf(os.Stderr, "stele review: %v\n", err)
			return 2
		}
	} else {
		done := 0
		res = review.Batches(ctx, m, batches, facts, *parallel, func(i int, b review.Batch, r *review.Result, err error) {
			done++
			if err != nil {
				fmt.Fprintf(out, "  [%d/%d] %s: failed: %v\n", done, len(batches), b.Label, err)
				return
			}
			fmt.Fprintf(out, "  [%d/%d] %s: %d finding(s)\n", done, len(batches), b.Label, len(r.Findings))
		})
		if len(res.Failed) == len(batches) {
			fmt.Fprintf(os.Stderr, "stele review: every review call failed\n")
			return 2
		}
	}
	js, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(*outPath, append(js, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "stele review: %v\n", err)
		return 2
	}
	for _, f := range res.Failed {
		fmt.Fprintf(out, "  failed: %s\n", f)
	}
	fmt.Fprintf(out, "%d contradiction(s) reported, %d unknown statement id(s); wrote %s\n", len(res.Findings), len(res.Unknown), *outPath)
	if res.Usage.CostUSD > 0 {
		fmt.Fprintf(out, "cost: about $%.2f\n", res.Usage.CostUSD)
	}
	return 0
}

func cmdRecall(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("recall", flag.ExitOnError)
	knownPath := fs.String("known", "", "known-conflicts file")
	stmtsPath := fs.String("statements", "", "statements file used by both systems")
	registry := fs.String("registry", "", "translated registry to check (from stele translate)")
	reviewPath := fs.String("review", "", "review JSON (from stele review)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	verbose := fs.Bool("v", false, "list every finding")
	_ = fs.Parse(args)
	if *knownPath == "" || *stmtsPath == "" || (*registry == "" && *reviewPath == "") {
		fmt.Fprint(os.Stderr, "usage: stele recall --known FILE --statements FILE [--registry DIR] [--review FILE] [-v]\n")
		return 2
	}
	known, err := score.LoadKnown(*knownPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele recall: %v\n", err)
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele recall: %v\n", err)
		return 2
	}
	var results []*score.RecallResult
	if *registry != "" {
		reg, err := ir.LoadDir(*registry)
		if err == nil {
			var prog *lower.Program
			if prog, err = lower.Lower(reg, lower.Options{IgnoreDecisions: true}); err == nil {
				var res *checker.Result
				if res, err = checker.Check(context.Background(), &solver.Z3{}, prog); err == nil {
					results = append(results, score.Recall("stele (translate + check)", known, score.CheckerFindings(res, stmts, reg), stmts))
					mt := score.Meeting(reg, stmts)
					fmt.Fprintf(out, "meeting rate: %d/%d translated statements share a symbol with a statement from another spec\n", mt.Meeting, mt.Translated)
					never := 0
					for _, d := range res.Dead {
						if d.NeverAllowed {
							never++
						}
					}
					fmt.Fprintf(out, "vocabulary: %d operation(s) never allowed (default deny, no rule allows them); not counted as findings\n", never)
				}
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "stele recall: %v\n", err)
			return 2
		}
	}
	if *reviewPath != "" {
		src, err := os.ReadFile(*reviewPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stele recall: %v\n", err)
			return 2
		}
		var rv review.Result
		if err := json.Unmarshal(src, &rv); err != nil {
			fmt.Fprintf(os.Stderr, "stele recall: %v\n", err)
			return 2
		}
		results = append(results, score.Recall("LLM-only review ("+rv.Backend+")", known, score.ReviewFindings(&rv, stmts), stmts))
	}
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return 0
	}
	for _, r := range results {
		fmt.Fprintf(out, "%s: %d/%d known conflicts found; %d finding(s), %d matching a known conflict\n", r.System, r.Found, len(r.Known), len(r.Findings), r.Matched)
		for _, k := range r.Known {
			mark := "missed"
			if k.Found {
				mark = fmt.Sprintf("found by #%d", k.By)
			}
			if k.Unsegmented {
				mark += " (a source line is not in the statements)"
			}
			fmt.Fprintf(out, "  %-3s %s\n", k.ID, mark)
		}
		if *verbose {
			for i, f := range r.Findings {
				fmt.Fprintf(out, "  #%d %s\n", i+1, f.Detail)
			}
		}
		fmt.Fprintln(out)
	}
	return 0
}
