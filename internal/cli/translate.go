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

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

func cmdTranslate(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("translate", flag.ExitOnError)
	vocab := fs.String("vocab", "", "comma-separated YAML files holding the vocabulary (entities, operations, world facts)")
	stmtsPath := fs.String("statements", "", "YAML list of statements: id, source, prose")
	outDir := fs.String("out", "", "directory to write the candidate registry and report.json into")
	backend := fs.String("llm", "", "LLM backend(s), comma-separated: anthropic (API key), claude (the CLI), cmd; default from stele.yaml, else anthropic")
	model := fs.String("model", "", "model for the backend (anthropic defaults to claude-opus-5-5; CLIs use their own default)")
	effort := fs.String("effort", "", "reasoning effort: low, medium, high, xhigh, max")
	command := fs.String("llm-cmd", "", "for --llm cmd: shell command that reads the prompt on stdin and prints the reply")
	repairs := fs.Int("max-repairs", 2, "rounds of feeding compile errors back to the model")
	batch := fs.Int("batch", 40, "statements per request (0 = all in one)")
	parallel := fs.Int("parallel", 4, "concurrent requests")
	samples := fs.Int("samples", 0, "drafts per requirement per backend; above 1 in total, readings are clustered and disagreements become questions; default from stele.yaml (translate.drafts), else 1")
	timeout := fs.Duration("timeout", 30*time.Minute, "limit for the whole run")
	_ = fs.Parse(args)
	if *vocab == "" || *stmtsPath == "" || *outDir == "" {
		fmt.Fprint(os.Stderr, "usage: stele translate --vocab domain.yaml --statements statements.yaml --out DIR [--llm anthropic|claude|cmd] [--model M]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele translate: %v\n", err)
		return 2
	}
	cfg, err := config.Find(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele translate: %v\n", err)
		return 2
	}
	if *backend == "" {
		*backend = "anthropic"
		if cfg != nil {
			*backend = cfg.Translate.LLM
		}
	}
	if *model == "" && cfg != nil {
		*model = cfg.Translate.Model
	}
	if *effort == "" && cfg != nil {
		*effort = cfg.Translate.Effort
	}
	if *samples == 0 {
		*samples = 1
		if cfg != nil {
			*samples = cfg.Translate.Drafts
		}
	}
	var clients []llm.Client
	for _, b := range strings.Split(*backend, ",") {
		b, md, err := llm.Resolve(strings.TrimSpace(b), *model)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stele translate: %v\n", err)
			return 2
		}
		e := *effort
		if e == "" {
			e = translationEffort[b]
		}
		client, err := llm.New(llm.Config{Backend: b, Model: md, Effort: e, Command: *command})
		if err != nil {
			fmt.Fprintf(os.Stderr, "stele translate: %v\n", err)
			return 2
		}
		clients = append(clients, client)
	}
	files := strings.Split(*vocab, ",")
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	names := make([]string, len(clients))
	for i, c := range clients {
		names[i] = c.Name()
	}
	fmt.Fprintf(out, "translating %d statement(s) with %s, %d sample(s) each ...\n", len(stmts), strings.Join(names, ", "), *samples)
	rep, err := translate.Translate(ctx, clients, stmts, translate.Options{VocabFiles: files, MaxRepairs: *repairs, BatchSize: *batch, Parallel: *parallel,
		Samples: *samples, Solver: &solver.Z3{}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele translate: %v\n", err)
		return 2
	}
	if err := translate.WriteRegistry(*outDir, rep, stmts, files); err != nil {
		fmt.Fprintf(os.Stderr, "stele translate: %v\n", err)
		return 2
	}
	counts := map[string]int{}
	for _, c := range rep.Candidates {
		counts[c.Outcome]++
	}
	fmt.Fprintf(out, "%d rule(s), %d informal, %d need vocabulary, %d invalid after %d round(s)\n",
		counts["rule"], counts["informal"], counts["needs_vocabulary"], rep.Invalid, rep.Rounds)
	if rep.Usage.CostUSD > 0 {
		fmt.Fprintf(out, "usage: %d in, %d cache write, %d cache read, %d out; about $%.2f\n",
			rep.Usage.InputTokens, rep.Usage.CacheWriteTokens, rep.Usage.CacheReadTokens, rep.Usage.OutputTokens, rep.Usage.CostUSD)
	}
	if len(rep.Clusters) > 0 {
		st := map[string]int{}
		for _, c := range rep.Clusters {
			st[c.Status]++
		}
		fmt.Fprintf(out, "clustering: %d agreed, %d ambiguous (see questions.md), %d with no valid translation\n", st[translate.Agreed], st[translate.Ambiguous], st[translate.Invalid])
	}
	for _, e := range rep.BatchErrors {
		fmt.Fprintf(out, "batch failed: %s\n", e)
	}
	fmt.Fprintf(out, "wrote %s (rules.yaml, report.json)\n", *outDir)
	return 0
}

func cmdScore(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	ref := fs.String("reference", "", "hand-written registry directory")
	cand := fs.String("candidate", "", "translated registry directory (from stele translate)")
	stmtsPath := fs.String("statements", "", "the statements file used for the translation")
	jsonOut := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	if *ref == "" || *cand == "" || *stmtsPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele score --reference DIR --candidate DIR --statements FILE [--json]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele score: %v\n", err)
		return 2
	}
	res, err := score.Score(context.Background(), &solver.Z3{}, *ref, *cand, stmts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele score: %v\n", err)
		return 2
	}
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return 0
	}
	for _, s := range res.Statements {
		fmt.Fprintf(out, "[%-12s] %s\n", s.Verdict, s.ID)
		if s.Verdict == score.Equivalent {
			continue
		}
		for _, r := range s.RefIR {
			fmt.Fprintf(out, "      reference  %s\n", r)
		}
		for _, c := range s.CandIR {
			fmt.Fprintf(out, "      candidate  %s\n", c)
		}
		if len(s.Witness) > 0 {
			parts := make([]string, len(s.Witness))
			for i, b := range s.Witness {
				parts[i] = b.Name + "=" + b.Value
			}
			fmt.Fprintf(out, "      they differ at: %s\n", strings.Join(parts, ", "))
		}
	}
	fmt.Fprintf(out, "\ntranslation: %d/%d statements equivalent to the reference\n", res.Equivalent, res.Total)
	fmt.Fprintf(out, "conflicts:   %d/%d reference conflicts found, %d extra\n", len(res.Found), len(res.Found)+len(res.Missed), len(res.Extra))
	for _, k := range res.Missed {
		fmt.Fprintf(out, "  missed  %s\n", k)
	}
	for _, k := range res.Extra {
		fmt.Fprintf(out, "  extra   %s\n", k)
	}
	return 0
}

// cmdAnswer applies an author's choice of reading to a translation run and
// rewrites its registry, report and questions.
func cmdAnswer(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("answer", flag.ExitOnError)
	run := fs.String("run", "", "translation run directory (from stele translate)")
	stmt := fs.String("statement", "", "statement id the question is about")
	reading := fs.Int("reading", 0, "the reading the author means, as numbered in questions.md")
	_ = fs.Parse(args)
	if *run == "" || *stmt == "" || *reading == 0 {
		fmt.Fprint(os.Stderr, "usage: stele answer --run DIR --statement ID --reading N\n")
		return 2
	}
	rep, err := translate.LoadRun(*run)
	if err == nil {
		err = translate.Answer(rep, *stmt, *reading)
	}
	var vocab []string
	if err == nil {
		vocab, err = translate.RunVocabulary(*run)
	}
	if err == nil {
		err = translate.WriteRegistry(*run, rep, rep.Inputs, vocab)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele answer: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "%s: reading %d is now active\n", *stmt, *reading)
	return 0
}
