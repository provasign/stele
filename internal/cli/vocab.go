package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// cmdVocab dispatches `stele vocab <subcommand>`.
func cmdVocab(args []string, out io.Writer) int {
	if len(args) == 0 || args[0] != "propose" {
		fmt.Fprint(os.Stderr, "usage: stele vocab propose --statements FILE --out proposed.yaml [--vocab files] [--context NAME] [--llm B]\n")
		return 2
	}
	fs := flag.NewFlagSet("vocab propose", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file (from stele import)")
	vocabFiles := fs.String("vocab", "", "comma-separated existing vocabulary files; may be just a context file")
	outPath := fs.String("out", "", "proposed vocabulary file to write; a report is written beside it")
	ctxName := fs.String("context", "", "context new symbols belong to (default: the vocabulary's first context, else main)")
	batch := fs.Int("batch", 40, "statements per request; batches run in order and see earlier proposals")
	facts := fs.Bool("facts", true, "first find the facts across all statements, one attribute per fact")
	timeout := fs.Duration("timeout", 60*time.Minute, "limit for the run")
	client := llmFlags(fs)
	_ = fs.Parse(args[1:])
	if *stmtsPath == "" || *outPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele vocab propose --statements FILE --out proposed.yaml [--vocab files]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("vocab propose", err)
	}
	cfg, err := config.Find(".")
	if err != nil {
		return failf("vocab propose", err)
	}
	if cfg != nil && !flagSet(fs, "llm") {
		b := cfg.Vocab.LLM
		if b == "" {
			b = cfg.Translate.LLM
		}
		_ = fs.Lookup("llm").Value.Set(b)
	}
	if cfg != nil && cfg.Vocab.Model != "" && !flagSet(fs, "model") {
		_ = fs.Lookup("model").Value.Set(cfg.Vocab.Model)
	}
	m, err := client()
	if err != nil {
		return failf("vocab propose", err)
	}
	var files []string
	if *vocabFiles != "" {
		files = strings.Split(*vocabFiles, ",")
	}
	if len(files) == 0 {
		// Start from nothing but a context.
		tmp, err := os.MkdirTemp("", "stele-vocab-ctx-")
		if err != nil {
			return failf("vocab propose", err)
		}
		defer os.RemoveAll(tmp)
		name := *ctxName
		if name == "" {
			name = "main"
		}
		f := filepath.Join(tmp, "context.yaml")
		if err := os.WriteFile(f, []byte("context: "+name+"\nformatVersion: 1\n"), 0o644); err != nil {
			return failf("vocab propose", err)
		}
		files = []string{f}
		*ctxName = name
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	fmt.Fprintf(out, "proposing vocabulary for %d statement(s) with %s ...\n", len(stmts), m.Name())
	rep, err := vocab.Propose(ctx, m, stmts, vocab.Options{VocabFiles: files, Context: *ctxName, BatchSize: *batch, Facts: *facts})
	if err != nil {
		return failf("vocab propose", err)
	}
	src, err := vocab.YAML(rep.Proposal, contextOrMain(*ctxName))
	if err != nil {
		return failf("vocab propose", err)
	}
	header := "# Proposed by `stele vocab propose`. Review before use: accept, rename, merge or delete.\n" +
		"# Possible duplicates are listed in the report beside this file.\n"
	if err := os.WriteFile(*outPath, append([]byte(header), src...), 0o644); err != nil {
		return failf("vocab propose", err)
	}
	js, _ := json.MarshalIndent(rep, "", "  ")
	reportPath := strings.TrimSuffix(*outPath, filepath.Ext(*outPath)) + ".report.json"
	if err := os.WriteFile(reportPath, append(js, '\n'), 0o644); err != nil {
		return failf("vocab propose", err)
	}
	p := rep.Proposal
	attrs := 0
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		attrs += len(e.Attributes)
	}
	if *facts {
		fmt.Fprintf(out, "%d fact(s), %d merged as duplicates\n", len(rep.Facts), len(rep.Merges))
	}
	fmt.Fprintf(out, "%d new entit(ies), %d extension(s), %d attribute(s), %d operation(s); %d possible duplicate(s); %d batch(es) failed\n",
		len(p.Entities), len(p.Extensions), attrs, len(p.Operations), len(rep.Flags), len(rep.Errors))
	for _, f := range rep.Flags {
		fmt.Fprintf(out, "  check %s: %s %s\n", f.Symbol, f.Reason, f.Similar)
	}
	if rep.Usage.CostUSD > 0 {
		fmt.Fprintf(out, "about $%.2f\n", rep.Usage.CostUSD)
	}
	fmt.Fprintf(out, "wrote %s and %s\n", *outPath, reportPath)
	return 0
}

func contextOrMain(s string) string {
	if s == "" {
		return "main"
	}
	return s
}

func failf(cmd string, err error) int {
	fmt.Fprintf(os.Stderr, "stele %s: %v\n", cmd, err)
	return 2
}

func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}
