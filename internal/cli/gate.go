package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/solver"
)

// cmdGate is gate 1 for CI: base is the registry on the target branch, head
// the registry in the pull request.
func cmdGate(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	base := fs.String("base", "", "registry directory on the target branch (empty for a new registry)")
	head := fs.String("head", "", "registry directory in the change")
	jsonOut := fs.Bool("json", false, "emit JSON")
	requireApproval := fs.Bool("require-approval", true, "block rules the change adds or edits that the owner has not approved (stele approve)")
	_ = fs.Parse(args)
	if *head == "" {
		fmt.Fprint(os.Stderr, "usage: stele gate --base DIR --head DIR [--json] [--require-approval=false]\n")
		return 2
	}
	res, err := gate.Dirs(context.Background(), &solver.Z3{}, *base, *head)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stele gate: %v\n", err)
		return 2
	}
	if *requireApproval && len(res.Unapproved) > 0 {
		res.Accepted = false
	}
	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		for _, c := range res.Rules {
			fmt.Fprintf(out, "  %-14s %s %s\n", c.Kind, c.Symbol, c.Detail)
		}
		for _, c := range res.Vocab {
			fmt.Fprintf(out, "  %-14s %s %s\n", c.Kind, c.Symbol, c.Detail)
		}
		for _, c := range res.NeedsApproval {
			fmt.Fprintf(out, "[APPROVAL] %s (%s %s) changes the meaning of: %s\n", c.Symbol, c.Kind, c.Detail, strings.Join(c.Affected, ", "))
		}
		for _, e := range res.Errors {
			fmt.Fprintf(out, "[ERROR] %s\n", e)
		}
		for _, id := range res.Unapproved {
			fmt.Fprintf(out, "[UNAPPROVED] %s: the owner has not approved it for its current statement text (stele approve)\n", id)
		}
		if res.Check != nil {
			for _, c := range res.Check.Conflicts {
				fmt.Fprintf(out, "[CONFLICT] %s\n", strings.Join(c.Rules, " + "))
			}
			for _, d := range res.Check.Dead {
				fmt.Fprintf(out, "[DEAD] %s can never happen: %s\n", d.Response, strings.Join(d.Rules, " + "))
			}
		}
		printOpen(out, res.Open)
		if res.Accepted {
			fmt.Fprintln(out, "gate: pass")
		} else {
			fmt.Fprintln(out, "gate: blocked")
		}
	}
	if !res.Accepted {
		return 1
	}
	return 0
}

// printOpen lists open conflicts oldest first (docs/08 W5).
func printOpen(out io.Writer, open []gate.OpenStatus) {
	count := map[string]int{}
	for _, o := range open {
		count[o.Status]++
		what := strings.Join(o.Rules, " + ")
		switch {
		case o.Response != "" && what == "":
			what = o.Response + " can never happen"
		case o.Response != "":
			what = o.Response + " can never happen: " + what
		}
		switch o.Status {
		case gate.OpenTolerated:
			fmt.Fprintf(out, "[OPEN] %s since %s: %s\n", o.ID, o.Since, what)
		case gate.OpenStale:
			fmt.Fprintf(out, "[STALE] %s: a pinned rule changed, so it tolerates nothing; resolve the conflict or re-baseline: %s\n", o.ID, what)
		case gate.OpenResolved:
			fmt.Fprintf(out, "[RESOLVED] %s is no longer found; close it (status: closed): %s\n", o.ID, what)
		}
	}
	if n := len(open) - count[gate.OpenClosed]; n > 0 {
		fmt.Fprintf(out, "%d open conflict(s): %d tolerated, %d stale, %d resolved\n", n, count[gate.OpenTolerated], count[gate.OpenStale], count[gate.OpenResolved])
	}
}
