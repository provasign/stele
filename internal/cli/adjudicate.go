package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/provasign/stele/internal/adjudicate"
	"github.com/provasign/stele/internal/codegraph"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// cmdAdjudicate decides review candidates from the project's code (D-055):
// which statements the code follows, and whether those are the newer ones.
func cmdAdjudicate(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("adjudicate", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file")
	candPath := fs.String("review", "", "candidates: `stele review --out` JSON, or any JSON with a conflicts list of {statement_ids, situation}")
	repo := fs.String("repo", "", "the project's git working tree (statement sources are relative to it)")
	outPath := fs.String("out", "", "verdicts JSON to write")
	rounds := fs.Int("rounds", 3, "code search rounds per candidate")
	maxChars := fs.Int("max-chars", 30000, "size of the code excerpts shown per round")
	ranges := fs.Bool("ranges", true, "let the verdict call ask to read file line ranges")
	defs := fs.Bool("definitions", false, "also show where searched names are defined (experimental)")
	graph := fs.Bool("graph", false, "find definitions in Grove's code graph, as whole bodies (experimental; implies --definitions)")
	parallel := fs.Int("parallel", 3, "candidates decided at a time")
	noCode := fs.Bool("no-code", false, "ablation: decide from the statements alone")
	noDates := fs.Bool("no-dates", false, "ablation: do not show when each statement was written")
	only := fs.String("only", "", "comma-separated 1-based candidate numbers to decide")
	checkEdits := fs.Bool("check-edits", true, "check each drafted spec edit clause by clause against the code (one more call per decisive verdict; --check-edits=false to skip)")
	from := fs.String("from", "", "reuse the verdicts in this file instead of deciding again (with --check-edits)")
	client := llmFlags(fs)
	_ = fs.Parse(args)
	if *stmtsPath == "" || *candPath == "" || *repo == "" || *outPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele adjudicate --statements FILE --review review.json --repo DIR --out verdicts.json [--llm B]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("adjudicate", err)
	}
	src, err := os.ReadFile(*candPath)
	if err != nil {
		return failf("adjudicate", err)
	}
	var file struct {
		Conflicts []adjudicate.Candidate `json:"conflicts"`
	}
	if err := json.Unmarshal(src, &file); err != nil {
		return failf("adjudicate", fmt.Errorf("%s: %w", *candPath, err))
	}
	cands := file.Conflicts
	if *only != "" {
		pick := map[int]bool{}
		for _, f := range strings.Split(*only, ",") {
			var n int
			if _, err := fmt.Sscan(strings.TrimSpace(f), &n); err == nil {
				pick[n] = true
			}
		}
		var kept []adjudicate.Candidate
		for i, c := range cands {
			if pick[i+1] {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	m, err := client()
	if err != nil {
		return failf("adjudicate", err)
	}
	start := time.Now()
	opt := adjudicate.Options{
		Repo: *repo, Rounds: *rounds, NoCode: *noCode, NoDates: *noDates, Parallel: *parallel, Ranges: *ranges,
		Evidence: adjudicate.EvidenceOptions{MaxChars: *maxChars, NoDefinitions: !*defs},
	}
	if *graph {
		g, err := codegraph.Open(context.Background(), *repo)
		if err != nil {
			return failf("adjudicate", err)
		}
		defer g.Close()
		opt.Evidence.Define, opt.Evidence.NoDefinitions = g.Define, false
	}
	var verdicts []adjudicate.Verdict
	var usage llm.Usage
	if *from != "" {
		src, err := os.ReadFile(*from)
		if err != nil {
			return failf("adjudicate", err)
		}
		var prev struct {
			Verdicts []adjudicate.Verdict `json:"verdicts"`
		}
		if err := json.Unmarshal(src, &prev); err != nil {
			return failf("adjudicate", fmt.Errorf("%s: %w", *from, err))
		}
		for _, v := range prev.Verdicts {
			if *only == "" || slices.Contains(strings.Split(*only, ","), fmt.Sprint(v.Candidate)) {
				verdicts = append(verdicts, v)
			}
		}
		fmt.Fprintf(out, "reusing %d verdict(s) from %s\n", len(verdicts), *from)
	} else {
		fmt.Fprintf(out, "deciding %d candidate(s) with %s against %s ...\n", len(cands), m.Name(), *repo)
		verdicts, usage = adjudicate.Decide(context.Background(), m, stmts, cands, opt, func(v adjudicate.Verdict) {
			fmt.Fprintf(out, "  [%d] %s: %s\n", v.Candidate, v.Outcome, oneLine(v.Action, 160))
		})
	}
	if *checkEdits {
		fmt.Fprintf(out, "checking the drafted edits against the code ...\n")
		usage.Add(adjudicate.CheckEdits(context.Background(), m, stmts, verdicts, opt, func(v adjudicate.Verdict) {
			mark := "kept"
			if v.EditChanged {
				mark = "changed"
			}
			fmt.Fprintf(out, "  [%d] edit %s: %s\n", v.Candidate, mark, oneLine(v.Edit, 160))
		}))
	}
	counts := adjudicate.Counts(verdicts)
	js, _ := json.MarshalIndent(struct {
		Backend  string               `json:"backend"`
		Seconds  float64              `json:"seconds"`
		CostUSD  float64              `json:"cost_usd"`
		NoCode   bool                 `json:"no_code,omitempty"`
		NoDates  bool                 `json:"no_dates,omitempty"`
		Counts   map[string]int       `json:"counts"`
		Verdicts []adjudicate.Verdict `json:"verdicts"`
	}{m.Name(), time.Since(start).Seconds(), usage.CostUSD, *noCode, *noDates, counts, verdicts}, "", "  ")
	if err := os.WriteFile(*outPath, append(js, '\n'), 0o644); err != nil {
		return failf("adjudicate", err)
	}
	var parts []string
	for _, k := range adjudicate.SortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	fmt.Fprintf(out, "%d candidate(s): %s; about $%.2f, %.0fs; wrote %s\n", len(verdicts), strings.Join(parts, ", "), usage.CostUSD, time.Since(start).Seconds(), *outPath)
	return 0
}

// cmdHygiene runs the structural pass: restated titles, misplaced spec
// folders, and names statements write as code that the code lacks.
func cmdHygiene(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("hygiene", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file")
	repo := fs.String("repo", "", "the project's git working tree")
	outPath := fs.String("out", "", "JSON to write (default: text to stdout only)")
	_ = fs.Parse(args)
	if *stmtsPath == "" || *repo == "" {
		fmt.Fprint(os.Stderr, "usage: stele hygiene --statements FILE --repo DIR [--out hygiene.json]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("hygiene", err)
	}
	h, err := adjudicate.CheckHygiene(*repo, stmts)
	if err != nil {
		return failf("hygiene", err)
	}
	fmt.Fprintf(out, "%d title(s) restated with different text, %d repeated verbatim, %d misplaced folder(s), %d code name(s) not in the code\n",
		len(h.Restated), len(h.Repeated), len(h.Misplaced), len(h.Unknown))
	for _, r := range h.Restated {
		fmt.Fprintf(out, "  restated: %s: %s (%d copies)\n", r.File, r.Title, len(r.IDs))
	}
	for _, m := range h.Misplaced {
		fmt.Fprintf(out, "  misplaced: %s\n", m)
	}
	for _, u := range h.Unknown {
		fmt.Fprintf(out, "  not in code: `%s` (%s)\n", u.Name, u.StatementID)
	}
	if *outPath != "" {
		js, _ := json.MarshalIndent(h, "", "  ")
		if err := os.WriteFile(*outPath, append(js, '\n'), 0o644); err != nil {
			return failf("hygiene", err)
		}
	}
	return 0
}

// oneLine joins s onto one line and cuts it at n characters. Cut at n
// bytes, it split a character in two and left the report invalid UTF-8 (a
// case-study adoption, 2026-11-11).
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
