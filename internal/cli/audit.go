package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/provasign/stele/internal/adjudicate"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/translate"
)

// cmdAudit runs the D-055 pipeline on one repository: import, hygiene,
// review, adjudicate, and a report for the spec's owners. A step whose
// output already exists in --out is not run again, so an interrupted audit
// resumes where it stopped.
func cmdAudit(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	format := fs.String("format", "auto", "spec layout: auto, openspec, speckit, kiro, markdown")
	outDir := fs.String("out", "", "directory for statements.yaml, hygiene.json, review.json, verdicts.json and report.md")
	budget := fs.Int("budget-tokens", 150000, "review: split a corpus larger than this into area reviews")
	parallel := fs.Int("parallel", 3, "adjudicate: candidates decided at a time")
	rounds := fs.Int("rounds", 3, "adjudicate: code search rounds per candidate")
	ranges := fs.Bool("ranges", true, "adjudicate: let the verdict call ask to read file line ranges")
	checkEdits := fs.Bool("check-edits", true, "adjudicate: check each drafted edit clause by clause against the code")
	fresh := fs.Bool("fresh", false, "run every step again, even when its output exists")
	backend := fs.String("llm", "anthropic", "LLM backend: anthropic (API key), claude (the CLI), cmd")
	model := fs.String("model", "", "model for the backend")
	effort := fs.String("effort", "", "reasoning effort")
	command := fs.String("llm-cmd", "", "for --llm cmd: shell command that reads the prompt on stdin and prints the reply")
	// Accept `stele audit <repo> --flags`: flag parsing stops at the first positional.
	repo := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		repo, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		repo = fs.Arg(0)
	}
	if repo == "" || *outDir == "" {
		fmt.Fprint(os.Stderr, "usage: stele audit <repo> --out DIR [--format auto|openspec|speckit|kiro|markdown] [--llm B --model M]\n")
		return 2
	}
	if *format == "auto" {
		*format = detectFormat(repo)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return failf("audit", err)
	}
	llmArgs := []string{"--llm", *backend}
	if *model != "" {
		llmArgs = append(llmArgs, "--model", *model)
	}
	if *effort != "" {
		llmArgs = append(llmArgs, "--effort", *effort)
	}
	if *command != "" {
		llmArgs = append(llmArgs, "--llm-cmd", *command)
	}
	path := func(name string) string { return filepath.Join(*outDir, name) }
	step := func(name, file string, run func() int) bool {
		if _, err := os.Stat(path(file)); err == nil && !*fresh {
			fmt.Fprintf(out, "%s: %s exists, not run again\n", name, path(file))
			return true
		}
		fmt.Fprintf(out, "%s:\n", name)
		return run() == 0
	}
	stmtsPath, reviewPath, verdictsPath := path("statements.yaml"), path("review.json"), path("verdicts.json")
	if !step("import "+*format, "statements.yaml", func() int {
		a := []string{*format, repo, "--out", stmtsPath}
		if *format == "openspec" {
			a = append(a, "--dates")
		}
		return cmdImport(a, out)
	}) {
		return 2
	}
	if !step("hygiene", "hygiene.json", func() int {
		return cmdHygiene([]string{"--statements", stmtsPath, "--repo", repo, "--out", path("hygiene.json")}, out)
	}) {
		return 2
	}
	if !step("review", "review.json", func() int {
		return cmdReview(append([]string{"--statements", stmtsPath, "--out", reviewPath, "--budget-tokens", strconv.Itoa(*budget)}, llmArgs...), out)
	}) {
		return 2
	}
	if !step("adjudicate", "verdicts.json", func() int {
		return cmdAdjudicate(append([]string{"--statements", stmtsPath, "--review", reviewPath, "--repo", repo, "--out", verdictsPath,
			"--parallel", strconv.Itoa(*parallel), "--rounds", strconv.Itoa(*rounds), "--ranges=" + strconv.FormatBool(*ranges), "--check-edits=" + strconv.FormatBool(*checkEdits)}, llmArgs...), out)
	}) {
		return 2
	}
	report, findings, err := auditReport(repo, stmtsPath, path("hygiene.json"), reviewPath, verdictsPath)
	if err != nil {
		return failf("audit", err)
	}
	if err := os.WriteFile(path("report.md"), []byte(report), 0o644); err != nil {
		return failf("audit", err)
	}
	fmt.Fprintf(out, "%d finding(s) for the spec's owners; wrote %s\n", findings, path("report.md"))
	if findings > 0 {
		return 1
	}
	return 0
}

// detectFormat names the spec layout of repo, by the directories each
// importer reads.
func detectFormat(repo string) string {
	isDir := func(p string) bool {
		fi, err := os.Stat(filepath.Join(repo, p))
		return err == nil && fi.IsDir()
	}
	switch {
	case isDir("openspec/specs"):
		return "openspec"
	case isDir(".kiro/specs"):
		return "kiro"
	case isDir(".specify") || isDir("specs"):
		if m, _ := filepath.Glob(filepath.Join(repo, "specs", "*", "spec.md")); len(m) > 0 {
			return "speckit"
		}
	}
	return "markdown"
}

// auditReport writes the findings of an audit as Markdown, most urgent
// first, and counts those that ask an owner to act.
func auditReport(repo, stmtsPath, hygienePath, reviewPath, verdictsPath string) (string, int, error) {
	stmts, err := translate.LoadStatements(stmtsPath)
	if err != nil {
		return "", 0, err
	}
	byID := map[string]translate.Statement{}
	for _, s := range stmts {
		byID[s.ID] = s
	}
	var h adjudicate.Hygiene
	var rv review.Result
	var vf struct {
		CostUSD  float64              `json:"cost_usd"`
		Verdicts []adjudicate.Verdict `json:"verdicts"`
	}
	for _, f := range []struct {
		path string
		v    any
	}{{hygienePath, &h}, {reviewPath, &rv}, {verdictsPath, &vf}} {
		src, err := os.ReadFile(f.path)
		if err != nil {
			return "", 0, err
		}
		if err := json.Unmarshal(src, f.v); err != nil {
			return "", 0, fmt.Errorf("%s: %w", f.path, err)
		}
	}
	counts := adjudicate.Counts(vf.Verdicts)
	var b strings.Builder
	fmt.Fprintf(&b, "# Spec audit: %s\n\n", filepath.Base(filepath.Clean(repo)))
	fmt.Fprintf(&b, "%d statements; %d suspected conflict(s) from the review, each decided against the code. LLM cost about $%.2f.\n\n",
		len(stmts), len(rv.Findings), rv.Usage.CostUSD+vf.CostUSD)
	var parts []string
	for _, k := range adjudicate.SortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, "Verdicts: %s.\n\n", strings.Join(parts, ", "))
	}
	sections := []struct {
		outcome, title, note string
	}{
		{"older_followed", "The code follows the older statement", "The newer statement is not implemented, or the code regressed. Decide which one is intended."},
		{"overbroad", "A statement is broader than the code", "Narrow the statement, or add the exception the code makes."},
		{"newer_followed", "Stale statements", "The code follows the newer statement; update or retire the older one."},
		{"followed_undated", "Stale statements (no dates)", "The code follows one statement; the others could not be dated."},
		{"unclear", "Not decided from the code", "Read these yourself, or run the formal path on them."},
	}
	findings := 0
	for _, sec := range sections {
		var vs []adjudicate.Verdict
		for _, v := range vf.Verdicts {
			if v.Outcome == sec.outcome {
				vs = append(vs, v)
			}
		}
		if len(vs) == 0 {
			continue
		}
		if sec.outcome != "unclear" {
			findings += len(vs)
		}
		fmt.Fprintf(&b, "## %s (%d)\n\n%s\n\n", sec.title, len(vs), sec.note)
		for _, v := range vs {
			fmt.Fprintf(&b, "### Candidate %d\n\n", v.Candidate)
			for _, id := range v.StatementIDs {
				role := "followed"
				switch {
				case slices.Contains(v.Stale, id):
					role = "stale"
				case slices.Contains(v.Overbroad, id):
					role = "over-broad"
				case !slices.Contains(v.Follows, id):
					role = "-"
				}
				date := v.Dates[id].Date
				if date == "" {
					date = "undated"
				}
				fmt.Fprintf(&b, "- **%s** (%s, %s): `%s`\n", id, role, date, byID[id].Source)
			}
			switch {
			case v.Edit != "":
				fmt.Fprintf(&b, "\n**Edit:** %s\n", v.Edit)
			case v.Action != "":
				fmt.Fprintf(&b, "\n**Action:** %s\n", v.Action)
				if v.EditNote != "" {
					fmt.Fprintf(&b, "\n**Not checked against the code:** %s\n", v.EditNote)
				}
			}
			fmt.Fprintf(&b, "\n**Why:** %s\n", v.Reason)
			var cites []string
			for _, c := range v.Cites {
				if c.Valid {
					cites = append(cites, fmt.Sprintf("`%s:%d`", c.File, c.Line))
				}
			}
			if len(cites) > 0 {
				fmt.Fprintf(&b, "\n**Code:** %s\n", strings.Join(cites, ", "))
			}
			b.WriteString("\n")
		}
	}
	var other []string
	for _, v := range vf.Verdicts {
		switch v.Outcome {
		case "compatible", "resolved_in_text", "not_implemented":
			other = append(other, fmt.Sprintf("- Candidate %d, %s: %s", v.Candidate, v.Outcome, oneLine(v.Reason, 200)))
		}
	}
	if len(other) > 0 {
		fmt.Fprintf(&b, "## No conflict in the code (%d)\n\n%s\n\n", len(other), strings.Join(other, "\n"))
	}
	if n := len(h.Restated) + len(h.Misplaced) + len(h.Unknown); n > 0 {
		findings += len(h.Restated) + len(h.Misplaced)
		fmt.Fprintf(&b, "## Spec hygiene\n\n")
		for _, r := range h.Restated {
			fmt.Fprintf(&b, "- Restated with different text: %s, %q (%s)\n", r.File, r.Title, strings.Join(r.IDs, ", "))
		}
		for _, m := range h.Misplaced {
			fmt.Fprintf(&b, "- Folder with no spec of its own: %s\n", m)
		}
		names := map[string][]string{}
		for _, u := range h.Unknown {
			names[u.Name] = append(names[u.Name], u.StatementID)
		}
		keys := make([]string, 0, len(names))
		for k := range names {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "- Not in the code: `%s` (%s)\n", k, strings.Join(names[k], ", "))
		}
		b.WriteString("\n")
	}
	return b.String(), findings, nil
}
