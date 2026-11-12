package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/formalize"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lesson"
	"github.com/provasign/stele/internal/translate"
)

// cmdFormalize builds a registry from a whole corpus of statements.
func cmdFormalize(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("formalize", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file (from stele import)")
	outDir := fs.String("out", "", "registry directory to write; a run that stopped resumes from it")
	vocabFiles := fs.String("vocab", "", "comma-separated existing vocabulary files (default: start from an empty context)")
	ctxName := fs.String("context", "main", "context new symbols belong to")
	rounds := fs.Int("rounds", 3, "gap-closing rounds: missing terms go back to the vocabulary step, then those statements are translated again")
	batch := fs.Int("batch", 40, "statements per translation and vocabulary request")
	parallel := fs.Int("parallel", 4, "concurrent model requests: translation batches (compact: one wave of document batches at a time) and consolidation")
	noPrune := fs.Bool("no-prune", false, "keep vocabulary no rule uses")
	onePass := fs.Bool("one-pass", false, "build vocabulary and rules together, one request per 100 statements, instead of vocabulary first")
	factsFirst := fs.Bool("facts-first", false, "compact mode: first propose the vocabulary's facts over the whole corpus, so every request shares one name per fact")
	compact := fs.Bool("compact", false, "joint vocabulary/rules with compact replies and statement-level repair (experimental; default batch 20)")
	batchBytes := fs.Int("batch-bytes", 12000, "compact mode: maximum source bytes per request (one oversized statement stays intact)")
	base := fs.String("base", "", "an existing registry the statements are added to: its vocabulary is reused (each request shows only the related part) and its rules are copied in, so the output is the combined registry")
	retrieve := fs.Int("retrieve", 0, "compact mode: show each request only this many related existing attributes (default 60 with --base)")
	across := fs.Bool("across-documents", false, "compact mode: let one request hold statements from several documents (with a large --batch and --batch-bytes, the corpus in one request)")
	maxCalls := fs.Int("max-calls", 0, "maximum total recorded model calls in this output directory; 0 is unlimited")
	maxCost := fs.Float64("max-cost", 0, "spend at most this many dollars on this run's model calls (as the backend reports): a call starts only when its expected cost, estimated from the calls so far, fits with the calls in flight; only the first call can pass it; 0 is unlimited")
	requestTimeout := fs.Duration("request-timeout", 0, "limit per model call; compact mode defaults to 3m")
	noCons := fs.Bool("no-consolidate", false, "skip merging the attributes rules use that name one fact twice")
	recon := fs.Bool("reconsolidate", false, "after the vocabulary batches, merge attributes that name one fact twice across the whole proposal")
	timeout := fs.Duration("timeout", 6*time.Hour, "limit for the run")
	factMap := fs.Bool("fact-map", true, "consolidation also asks once, over every used attribute, which name one fact and which facts tie others")
	codeCtx := fs.String("code-context", "", "compact mode: a file describing the code's structure (types, steps), to build the vocabulary on (D-053)")
	selfFlag := fs.Bool("self-check", true, "after translation, check each statement's rules against each other with the solver; a statement whose rules contradict each other is read once more, and its rules are held out if they still do")
	lessonsDir := fs.String("lessons", "", "a registry directory whose lessons (lessons/*.yaml, what the owner taught Stele, D-063) every request follows; default: the --base registry's")
	coverFlag := fs.Bool("cover", true, "after translation, check which clause of each statement its rules cover; statements with an uncovered clause are translated once more with the clause named, and stay partial if it remains uncovered")
	client := llmFlags(fs)
	_ = fs.Parse(args)
	if *compact && *onePass {
		return failf("formalize", fmt.Errorf("choose --compact or --one-pass, not both"))
	}
	if *factsFirst && !*compact {
		return failf("formalize", fmt.Errorf("--facts-first is for --compact; without --compact, formalize is facts-first already"))
	}
	if *batch <= 0 || *batchBytes <= 0 || *maxCalls < 0 || *requestTimeout < 0 {
		return failf("formalize", fmt.Errorf("batch and batch-bytes must be positive; max-calls and request-timeout cannot be negative"))
	}
	if *compact && !flagSet(fs, "batch") {
		*batch = 20
	}
	if *compact && !flagSet(fs, "request-timeout") {
		*requestTimeout = 3 * time.Minute
	}
	if *stmtsPath == "" || *outDir == "" {
		fmt.Fprint(os.Stderr, "usage: stele formalize --statements FILE --out DIR [--vocab files] [--context NAME] [--rounds N] [--llm B]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("formalize", err)
	}
	cfg, err := config.Find(".")
	if err != nil {
		return failf("formalize", err)
	}
	if cfg != nil && !flagSet(fs, "llm") && cfg.Translate.LLM != "" {
		_ = fs.Lookup("llm").Value.Set(cfg.Translate.LLM)
	}
	if cfg != nil && cfg.Translate.Model != "" && !flagSet(fs, "model") {
		_ = fs.Lookup("model").Value.Set(cfg.Translate.Model)
	}
	if cfg != nil && cfg.Translate.Effort != "" && !flagSet(fs, "effort") {
		_ = fs.Lookup("effort").Value.Set(cfg.Translate.Effort)
	}
	m, err := client()
	if err != nil {
		return failf("formalize", err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return failf("formalize", err)
	}
	var files []string
	if *base != "" {
		// The base's files are copied in under base- names: vocabulary and
		// world facts as vocabulary files, its rules as rules of the combined
		// registry (not shown to the model).
		entries, err := filepath.Glob(filepath.Join(*base, "*.yaml"))
		if err != nil || len(entries) == 0 {
			return failf("formalize", fmt.Errorf("--base %s: no registry files", *base))
		}
		for _, f := range entries {
			name := filepath.Base(f)
			dst := filepath.Join(*outDir, "base-"+name)
			src, err := os.ReadFile(f)
			if err != nil {
				return failf("formalize", err)
			}
			if err := os.WriteFile(dst, src, 0o644); err != nil {
				return failf("formalize", err)
			}
			if name != "rules.yaml" {
				files = append(files, dst)
			}
		}
		if *retrieve == 0 {
			*retrieve = 60
		}
	} else if *vocabFiles != "" {
		files = strings.Split(*vocabFiles, ",")
	} else {
		f := filepath.Join(*outDir, "context.yaml")
		if _, err := os.Stat(f); os.IsNotExist(err) {
			if err := os.WriteFile(f, []byte("context: "+*ctxName+"\nformatVersion: 1\n"), 0o644); err != nil {
				return failf("formalize", err)
			}
		} else if err != nil {
			return failf("formalize", err)
		}
		files = []string{f}
	}
	var codeText string
	if *codeCtx != "" {
		b, err := os.ReadFile(*codeCtx)
		if err != nil {
			return failf("formalize", err)
		}
		codeText = string(b)
	}
	from := *lessonsDir
	if from == "" {
		from = *base
	}
	var lessons []*ir.Lesson
	if from != "" {
		if lessons, err = lesson.Load(from); err != nil {
			return failf("formalize", err)
		}
		if len(lessons) > 0 {
			fmt.Fprintf(out, "following %d lesson(s) from %s\n", len(lessons), filepath.Join(from, lesson.Dir))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	fmt.Fprintf(out, "formalizing %d statement(s) with %s into %s (stele %s)\n", len(stmts), m.Name(), *outDir, buildRevision())
	onePassChunk := 100
	if flagSet(fs, "batch") {
		onePassChunk = *batch
	}
	res, err := formalize.Run(ctx, m, stmts, *outDir, formalize.Options{VocabFiles: files, Context: *ctxName, Rounds: *rounds,
		VocabBatch: *batch, Batch: *batch, Parallel: *parallel, NoPrune: *noPrune, Reconsolidate: *recon, NoConsolidate: *noCons, OnePass: *onePass, OnePassChunk: onePassChunk,
		Compact: *compact, FactsFirst: *factsFirst, BatchBytes: *batchBytes, AcrossDocuments: *across, Retrieve: *retrieve, MaxCalls: *maxCalls, MaxCost: *maxCost, RequestTimeout: *requestTimeout, CodeContext: codeText, FactMap: *factMap, NoCover: !*coverFlag, NoSelfCheck: !*selfFlag, Lessons: lessons, Log: out})
	if err != nil {
		return failf("formalize", err)
	}
	if s := formalize.CoverageSummary(res); s != "" {
		fmt.Fprintln(out, s)
	}
	if s := formalize.SelfCheckSummary(res); s != "" {
		fmt.Fprintln(out, s)
	}
	h := res.Health
	where := "set STELE_KEEP_CALLS=1 to keep each request and reply"
	if formalize.KeepCalls() {
		where = "see requests.jsonl and requests/"
	}
	fmt.Fprintf(out, "%d model calls: %d returned, %d failed, %d interrupted; %s\n", res.Calls.Started, res.Calls.Completed, res.Calls.Failed, res.Calls.Interrupted, where)
	if len(res.Deferred) > 0 {
		fmt.Fprintf(out, "%d statement(s) deferred for review and excluded from rules; see coverage.md and formalize.json\n", len(res.Deferred))
	}
	fmt.Fprintf(out, "\n%d statements: %d translated, %d partial, %d informal, %d need vocabulary, %d invalid\n", h.Statements,
		res.Counts[formalize.Translated], res.Counts[formalize.Partial], res.Counts[formalize.Informal], res.Counts[formalize.NeedsVocabulary], res.Counts[formalize.Invalid])
	fmt.Fprintf(out, "vocabulary: %d attributes (%d shared by two or more statements), %d operations; %d statements meet a statement from another source\n",
		h.Attributes, h.SharedAttrs, h.Operations, h.MeetingOtherSrc)
	fmt.Fprintf(out, "%d writes inferred, %d unused symbols pruned, %d attributes merged, %d world facts proposed (%s), %d possible duplicates\n",
		len(res.Inferred), len(res.Pruned), len(res.Merges), len(res.Links), formalize.LinksFile, len(res.Flags))
	for _, e := range res.Errors {
		fmt.Fprintf(out, "error: %s\n", e)
	}
	if res.Usage.CostUSD > 0 {
		fmt.Fprintf(out, "about $%.2f\n", res.Usage.CostUSD)
	}
	fmt.Fprintf(out, "wrote %s: %s, rules.yaml, %s, %s\n", *outDir, formalize.VocabularyFile, formalize.ReportFile, formalize.CoverageFile)
	return 0
}

// buildRevision is the source revision the binary was built from, marked
// "+modified" when the tree had uncommitted changes, so a run's log shows
// which code produced it.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return gitRevision()
	}
	rev, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
			if len(rev) > 12 {
				rev = rev[:12]
			}
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return gitRevision() // go run does not stamp version-control information
	}
	if modified {
		rev += "+modified"
	}
	return rev
}

// gitRevision reads the revision of the working tree: what go run compiles.
func gitRevision() string {
	out, err := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	rev := strings.TrimSpace(string(out))
	if st, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output(); err == nil && len(strings.TrimSpace(string(st))) > 0 {
		rev += "+modified"
	}
	return rev
}
