package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/provasign/stele/internal/bench"
	"github.com/provasign/stele/internal/solver"
)

// runs collects repeated --run SET=DIR flags in order.
type runs [][2]string

func (r *runs) String() string { return fmt.Sprint(*r) }
func (r *runs) Set(v string) error {
	name, dir, ok := strings.Cut(v, "=")
	if !ok || name == "" || dir == "" {
		return fmt.Errorf("want SET=DIR, got %q", v)
	}
	*r = append(*r, [2]string{name, dir})
	return nil
}

func cmdBench(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	setsPath := fs.String("sets", "", "benchmark sets file")
	var rs runs
	fs.Var(&rs, "run", "SET=DIR: a formalize output directory to score on a set (repeatable)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	z3Path := fs.String("z3", "", "path to a z3 binary to run (default: Stele's own Z3, else z3 on PATH)")
	_ = fs.Parse(args)
	if *setsPath == "" || len(rs) == 0 {
		fmt.Fprint(os.Stderr, "usage: stele bench --sets FILE --run SET=DIR [--run SET=DIR ...] [--json]\n")
		return 2
	}
	sets, err := bench.LoadSets(*setsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele bench: %v\n", err)
		return 2
	}
	byName := map[string]bench.Set{}
	for _, s := range sets {
		byName[s.Name] = s
	}
	z := &solver.Z3{Path: *z3Path}
	var reports []*bench.Report
	for _, r := range rs {
		set, ok := byName[r[0]]
		if !ok {
			fmt.Fprintf(os.Stderr, "stele bench: no set %q in %s\n", r[0], *setsPath)
			return 2
		}
		rep, err := bench.Run(context.Background(), z, set, r[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "stele bench: %s: %v\n", r[1], err)
			return 2
		}
		reports = append(reports, rep)
	}
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(reports)
		return 0
	}
	for i, rep := range reports {
		if i > 0 {
			fmt.Fprintln(out)
		}
		printBench(out, rep)
	}
	return 0
}

func printBench(out io.Writer, rep *bench.Report) {
	held := ""
	if rep.HeldOut {
		held = " (held out)"
	}
	fmt.Fprintf(out, "%s%s: %s\n", rep.Set, held, rep.Run)
	if rep.States == nil {
		fmt.Fprintf(out, "  coverage:   unknown (no formalize.json)\n")
	} else {
		fmt.Fprintf(out, "  checked:    %d/%d statements", rep.Checked, rep.Statements)
		for _, k := range []string{"translated", "partial", "informal", "obligation", "needs_vocabulary", "invalid", "absent"} {
			if n := rep.States[k]; n > 0 {
				fmt.Fprintf(out, ", %d %s", n, k)
			}
		}
		fmt.Fprintln(out)
		for i, m := range rep.Missing {
			if i == 5 {
				fmt.Fprintf(out, "              … %d more missing terms\n", len(rep.Missing)-i)
				break
			}
			fmt.Fprintf(out, "              missing ×%d: %s\n", m.N, m.Term)
		}
	}
	if rep.LoadError != "" {
		fmt.Fprintf(out, "  registry:   does not compile: %s\n", rep.LoadError)
	}
	if rep.Recall != nil {
		fmt.Fprintf(out, "  conflicts:  %d/%d known conflicts in this set found", rep.Recall.Found, rep.InCorpus)
		if extra := len(rep.Recall.Findings) - rep.Recall.Matched; extra > 0 {
			fmt.Fprintf(out, "; %d other findings to adjudicate", extra)
		}
		fmt.Fprintln(out)
		for _, k := range rep.Recall.Known {
			if !k.Found && !k.Unsegmented {
				fmt.Fprintf(out, "              missed %s\n", k.ID)
			}
		}
	}
	if rep.Reference != nil {
		fmt.Fprintf(out, "  reference:  %d/%d statements equivalent\n", rep.Reference.Equivalent, rep.Reference.Total)
	}
	if t := rep.Timing; t != nil {
		fmt.Fprintf(out, "  backend:    %s\n", strings.Join(t.Backends, ", "))
		fmt.Fprintf(out, "  time:       %.1f s with calls in flight (%.1f s summed, longest %.1f s, up to %d at once)\n",
			t.ActiveSeconds, t.CallSeconds, t.LongestCall, t.MaxInFlight)
		fmt.Fprintf(out, "  calls:      %d started, %d completed, %d failed, %d interrupted; %d prompt bytes, %d reply bytes\n",
			t.Calls.Started, t.Calls.Completed, t.Calls.Failed, t.Calls.Interrupted, t.PromptBytes, t.ReplyBytes)
		if u := t.Usage; u.InputTokens+u.OutputTokens > 0 {
			fmt.Fprintf(out, "  tokens:     %d in, %d out, $%.2f\n", u.InputTokens, u.OutputTokens, u.CostUSD)
		}
	}
}
