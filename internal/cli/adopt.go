package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/specsync"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// Adopting an existing spec corpus (docs/08 W5): review discovers candidate
// conflicts, only their statements are modeled (candidates), and the checker
// confirms each, or reports it not reproduced or unproved (confirm). Triage
// groups them and drafts each decision from git dates, the owners decide, and
// the baseline writes the decisions and open conflicts; writeback notes the
// superseded statements in the specs, and sync follows later spec changes.

func loadReview(path string) (*review.Result, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rv review.Result
	if err := json.Unmarshal(src, &rv); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &rv, nil
}

func cmdCandidates(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("candidates", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file the review read")
	reviewPath := fs.String("review", "", "review JSON (from stele review)")
	outPath := fs.String("out", "", "statements file to write")
	_ = fs.Parse(args)
	if *stmtsPath == "" || *reviewPath == "" || *outPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele candidates --statements FILE --review review.json --out candidates.yaml\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("candidates", err)
	}
	rv, err := loadReview(*reviewPath)
	if err != nil {
		return failf("candidates", err)
	}
	cs := score.Candidates(rv, stmts)
	src, err := yaml.Marshal(cs)
	if err != nil {
		return failf("candidates", err)
	}
	header := fmt.Sprintf("# %d of %d statements, cited by %d review finding(s) in %s.\n", len(cs), len(stmts), len(rv.Findings), *reviewPath)
	if err := os.WriteFile(*outPath, append([]byte(header), src...), 0o644); err != nil {
		return failf("candidates", err)
	}
	fmt.Fprintf(out, "wrote %d of %d statements to %s\n", len(cs), len(stmts), *outPath)
	return 0
}

func cmdConfirm(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("confirm", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file the review read")
	reviewPath := fs.String("review", "", "review JSON (from stele review)")
	registry := fs.String("registry", "", "registry translated from the candidates (from stele translate)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args)
	if *stmtsPath == "" || *reviewPath == "" || *registry == "" {
		fmt.Fprint(os.Stderr, "usage: stele confirm --statements FILE --review review.json --registry DIR [--json]\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("confirm", err)
	}
	rv, err := loadReview(*reviewPath)
	if err != nil {
		return failf("confirm", err)
	}
	reg, err := ir.LoadDir(*registry)
	if err != nil {
		return failf("confirm", err)
	}
	prog, err := lower.Lower(reg, lower.Options{IgnoreDecisions: true})
	if err != nil {
		return failf("confirm", err)
	}
	res, err := checker.Check(context.Background(), &solver.Z3{}, prog)
	if err != nil {
		return failf("confirm", err)
	}
	findings := score.CheckerFindings(res, stmts, reg)
	cands := score.Confirm(rv, reg, findings, stmts)
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"candidates": cands, "checker_findings": findings})
		return 0
	}
	count := map[string]int{}
	for i, c := range cands {
		count[c.Verdict]++
		fmt.Fprintf(out, "%3d %-14s %s\n", i+1, c.Verdict, strings.Join(c.Statements, " + "))
		switch {
		case c.By > 0:
			fmt.Fprintf(out, "              checker #%d: %s\n", c.By, findings[c.By-1].Detail)
		case len(c.Unmodeled) > 0:
			fmt.Fprintf(out, "              no checked rule for: %s\n", strings.Join(c.Unmodeled, ", "))
		}
	}
	confirmedBy := map[int]bool{}
	for _, c := range cands {
		if c.By > 0 {
			confirmedBy[c.By] = true
		}
	}
	fmt.Fprintf(out, "\n%d candidate(s): %d confirmed, %d not reproduced, %d unproved; the checker found %d finding(s), %d of them confirming no candidate\n",
		len(cands), count[score.Confirmed], count[score.NotReproduced], count[score.Unproved], len(findings), len(findings)-len(confirmedBy))
	return 0
}

const triageHeader = `# Adoption triage (docs/08 W5 steps 5 and 6), written by stele triage.
# Items are grouped by a shared statement, largest group first. For each, set
# decision to one of:
#   accept   the newer statement supersedes the older ones: retires "retire"
#   reverse  the older statements win: retires "retire_if_reversed"
#   open     leave the conflict open; the baseline records it (D-044)
#   dismiss  not a conflict; a checker conflict among its rules still becomes open
# Edit "retire" before accepting to retire other rules. Rerunning stele triage
# keeps decisions and notes of items it finds again.
`

func cmdTriage(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	stmtsPath := fs.String("statements", "", "statements file the review read")
	reviewPath := fs.String("review", "", "review JSON (from stele review)")
	registry := fs.String("registry", "", "registry translated from the candidates")
	outPath := fs.String("out", "", "triage file to write; an existing one keeps its decisions")
	_ = fs.Parse(args)
	if *stmtsPath == "" || *reviewPath == "" || *registry == "" || *outPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele triage --statements FILE --review review.json --registry DIR --out triage.yaml\n")
		return 2
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("triage", err)
	}
	rv, err := loadReview(*reviewPath)
	if err != nil {
		return failf("triage", err)
	}
	reg, err := ir.LoadDir(*registry)
	if err != nil {
		return failf("triage", err)
	}
	prog, err := lower.Lower(reg, lower.Options{IgnoreDecisions: true})
	if err != nil {
		return failf("triage", err)
	}
	res, err := checker.Check(context.Background(), &solver.Z3{}, prog)
	if err != nil {
		return failf("triage", err)
	}
	cands := score.Confirm(rv, reg, score.CheckerFindings(res, stmts, reg), stmts)
	items := adopt.Draft(cands, res, reg, stmts)
	kept := 0
	if prev, err := loadTriage(*outPath); err == nil {
		kept = adopt.Carry(prev, items)
	} else if !os.IsNotExist(err) {
		return failf("triage", err)
	}
	if err := adopt.Preview(context.Background(), &solver.Z3{}, reg, res, items); err != nil {
		return failf("triage", err)
	}
	src, err := yaml.Marshal(items)
	if err != nil {
		return failf("triage", err)
	}
	if err := os.WriteFile(*outPath, append([]byte(triageHeader), src...), 0o644); err != nil {
		return failf("triage", err)
	}
	groups, directed, checkerOnly := map[string]bool{}, 0, 0
	for _, it := range items {
		groups[it.Group] = true
		if it.Newer != "" {
			directed++
		}
		if it.Verdict == adopt.CheckerOnly {
			checkerOnly++
		}
	}
	fmt.Fprintf(out, "wrote %d item(s) in %d group(s) to %s: %d from the review, %d checker-only; %d drafted from git dates, %d with no direction; %d decision(s) kept\n",
		len(items), len(groups), *outPath, len(items)-checkerOnly, checkerOnly, directed, len(items)-directed, kept)
	return 0
}

func loadTriage(path string) ([]adopt.Item, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []adopt.Item
	if err := yaml.Unmarshal(src, &items); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return items, nil
}

func cmdBaseline(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	registry := fs.String("registry", "", "registry to baseline; decisions and open conflicts are written into it")
	triagePath := fs.String("triage", "", "triage file (from stele triage, decided by the owners)")
	stmtsPath := fs.String("statements", "", "statements file")
	by := fs.String("by", "", "comma-separated owners acknowledging the open conflicts")
	since := fs.String("since", time.Now().Format("2006-01-02"), "date recorded on each open conflict")
	keep := fs.Bool("keep-pending", false, "record pending items as open conflicts instead of refusing")
	_ = fs.Parse(args)
	if *registry == "" || *triagePath == "" || *stmtsPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele baseline --registry DIR --triage triage.yaml --statements FILE --by OWNER[,OWNER] [--keep-pending]\n")
		return 2
	}
	items, err := loadTriage(*triagePath)
	if err != nil {
		return failf("baseline", err)
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("baseline", err)
	}
	var owners []string
	for _, o := range strings.Split(*by, ",") {
		if o = strings.TrimSpace(o); o != "" {
			owners = append(owners, o)
		}
	}
	res, err := adopt.Baseline(context.Background(), &solver.Z3{}, *registry, items, stmts, adopt.Options{By: owners, Since: *since, KeepPending: *keep})
	if err != nil {
		return failf("baseline", err)
	}
	fmt.Fprintf(out, "%d decision(s) retiring %d rule(s) in %s; %d accepted item(s) retire no rule (write-back only); %d open conflict(s) in %s; check with them: %s\n",
		res.Decisions, res.Retired, adopt.DecisionsFile, res.Recorded, res.Open, adopt.ConflictsFile, res.Verdict)
	if len(res.Remaining) > 0 {
		ids := make([]string, 0, len(res.Remaining))
		for id := range res.Remaining {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Fprintln(out, "accepted or reversed items whose statements still conflict (retiring a rule can expose conflicts it hid; retire more, or leave them open):")
		for _, id := range ids {
			fmt.Fprintf(out, "  %s: %d open conflict(s)\n", id, res.Remaining[id])
		}
	}
	if res.Verdict != checker.Pass {
		return 1
	}
	return 0
}

func cmdWriteback(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("writeback", flag.ExitOnError)
	repo := fs.String("repo", "", "spec repository the statements were imported from")
	triagePath := fs.String("triage", "", "triage file")
	stmtsPath := fs.String("statements", "", "statements file")
	dry := fs.Bool("dry-run", false, "print the notes without writing")
	_ = fs.Parse(args)
	if *repo == "" || *triagePath == "" || *stmtsPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele writeback --repo DIR --triage triage.yaml --statements FILE [--dry-run]\n")
		return 2
	}
	items, err := loadTriage(*triagePath)
	if err != nil {
		return failf("writeback", err)
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("writeback", err)
	}
	notes, err := adopt.WriteBack(*repo, items, stmts, *dry)
	if err != nil {
		return failf("writeback", err)
	}
	for _, n := range notes {
		fmt.Fprintf(out, "%s#L%d\n  %s\n", n.File, n.Line, strings.TrimSpace(n.Text))
	}
	verb := "wrote"
	if *dry {
		verb = "would write"
	}
	fmt.Fprintf(out, "%s %d note(s); open a pull request with them, then run stele sync\n", verb, len(notes))
	return 0
}

func cmdSync(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	oldPath := fs.String("old", "", "statements file the registry was built from")
	newPath := fs.String("new", "", "statements file from a fresh import")
	registry := fs.String("registry", "", "registry")
	outPath := fs.String("out", "", "write the added and changed statements here, to translate")
	apply := fs.Bool("apply", false, "update statement provenance and retire the rules of removed statements")
	_ = fs.Parse(args)
	// With no files named, stele.yaml names them: the accepted statements,
	// a fresh import of the specs, and the registry.
	var cfg *config.Config
	if *oldPath == "" && *newPath == "" && *registry == "" {
		c, err := config.Find(".")
		if err == nil && c != nil && c.Specs != nil {
			cfg = c
			*oldPath, *registry = c.SnapshotFile(), c.RegistryDir()
		}
	}
	if *oldPath == "" || (*newPath == "" && cfg == nil) || *registry == "" {
		fmt.Fprint(os.Stderr, "usage: stele sync [--apply]   (in a project whose stele.yaml names its specs)\n       stele sync --old statements.yaml --new statements.yaml --registry DIR [--out FILE] [--apply]\n")
		return 2
	}
	old, err := translate.LoadStatements(*oldPath)
	if err != nil {
		return failf("sync", err)
	}
	var cur []translate.Statement
	if cfg != nil {
		cur, err = importSpecs(cfg.Dir(), cfg.Specs)
	} else {
		cur, err = translate.LoadStatements(*newPath)
	}
	if err != nil {
		return failf("sync", err)
	}
	reg, err := ir.LoadDir(*registry)
	if err != nil {
		return failf("sync", err)
	}
	rep := adopt.Sync(old, cur, reg)
	specsync.ApprovedAsWritten(rep, cur, reg)
	if len(rep.Ambiguous) > 0 {
		ids := make([]string, 0, len(rep.Ambiguous))
		for id := range rep.Ambiguous {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(out, "ambiguous %s -> %s (identify the statement before applying sync)\n", id, strings.Join(rep.Ambiguous[id], ", "))
		}
		if *apply {
			return failf("sync", fmt.Errorf("ambiguous statement moves; no registry changes applied"))
		}
	}
	for _, id := range rep.Added {
		fmt.Fprintf(out, "added    %s  (translate it: W1)\n", id)
	}
	for _, id := range rep.Changed {
		fmt.Fprintf(out, "changed  %s  (re-translate and bump the version of %s)\n", id, strings.Join(rep.RulesOf[id], ", "))
	}
	moved := make([]string, 0, len(rep.Moved))
	for from := range rep.Moved {
		moved = append(moved, from)
	}
	sort.Strings(moved)
	for _, from := range moved {
		fmt.Fprintf(out, "moved    %s -> %s  (same text; its rules keep their IDs)\n", from, rep.Moved[from])
	}
	for _, id := range rep.Removed {
		fmt.Fprintf(out, "removed  %s\n", id)
	}
	fmt.Fprintf(out, "%d unchanged, %d added, %d changed, %d moved, %d removed; %d rule(s) of removed statements to retire\n",
		rep.Unchanged, len(rep.Added), len(rep.Changed), len(rep.Moved), len(rep.Removed), len(rep.Retire))
	if *outPath != "" {
		want := map[string]bool{}
		for _, id := range append(append([]string{}, rep.Added...), rep.Changed...) {
			want[id] = true
		}
		var todo []translate.Statement
		for _, s := range cur {
			if want[s.ID] {
				todo = append(todo, s)
			}
		}
		src, err := yaml.Marshal(todo)
		if err != nil {
			return failf("sync", err)
		}
		if err := os.WriteFile(*outPath, src, 0o644); err != nil {
			return failf("sync", err)
		}
		fmt.Fprintf(out, "wrote %d statement(s) to translate to %s\n", len(todo), *outPath)
	}
	if *apply && len(rep.Retire) > 0 {
		if err := adopt.RetireRules(reg, rep.Retire); err != nil {
			return failf("sync", err)
		}
		fmt.Fprintf(out, "retired %d rule(s); open conflicts they were in now report resolved\n", len(rep.Retire))
	}
	if *apply && len(rep.Sources) > 0 {
		if err := adopt.UpdateSources(reg, rep.Sources); err != nil {
			return failf("sync", err)
		}
		fmt.Fprintf(out, "updated provenance for %d rule(s); IDs and versions preserved\n", len(rep.Sources))
	}
	if *apply && cfg != nil {
		// Nothing left to translate: the specs as they are become the
		// accepted statements saved-spec sync compares with.
		if len(rep.Added)+len(rep.Changed)+len(rep.Ambiguous) == 0 {
			if err := writeSnapshot(cfg.SnapshotFile(), cur, cfg.Specs.Format); err != nil {
				return failf("sync", err)
			}
			fmt.Fprintf(out, "accepted statements updated (%d); commit them with the registry\n", len(cur))
		} else {
			fmt.Fprintln(out, "accepted statements kept: check the added and changed requirements with `stele change` first")
		}
	}
	return 0
}
