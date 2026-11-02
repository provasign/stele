package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// cmdVerify judges each conflict of a checked registry the way a careful
// reader of the specs would: real (the statements as written cannot all
// hold), resolved in the text, uncertain, or an artifact of the
// translation. It classifies; it never changes a rule or decides which
// statement wins. Dismissed conflicts are kept with their reason.
func cmdVerify(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	regDir := fs.String("registry", "", "checked registry directory")
	checkPath := fs.String("check", "", "its `stele check --json` output")
	stmtsPath := fs.String("statements", "", "the statements file it was formalized from")
	outPath := fs.String("out", "", "verdicts JSON to write")
	batch := fs.Int("batch", 8, "conflicts per request")
	parallel := fs.Int("parallel", 4, "concurrent requests")
	requestTimeout := fs.Duration("request-timeout", 10*time.Minute, "limit per model call")
	client := llmFlags(fs)
	_ = fs.Parse(args)
	if *regDir == "" || *checkPath == "" || *stmtsPath == "" || *outPath == "" {
		fmt.Fprint(os.Stderr, "usage: stele verify --registry DIR --check check.json --statements FILE --out verdicts.json [--llm B]\n")
		return 2
	}
	reg, err := ir.LoadDir(*regDir)
	if err != nil {
		return failf("verify", err)
	}
	src, err := os.ReadFile(*checkPath)
	if err != nil {
		return failf("verify", err)
	}
	var res checker.Result
	if err := json.Unmarshal(src, &res); err != nil {
		return failf("verify", err)
	}
	stmts, err := translate.LoadStatements(*stmtsPath)
	if err != nil {
		return failf("verify", err)
	}
	m, err := client()
	if err != nil {
		return failf("verify", err)
	}
	byID := map[string]translate.Statement{}
	for _, s := range stmts {
		byID[s.ID] = s
	}
	start := time.Now()
	verdicts, usage, err := verifyConflicts(context.Background(), m, reg, &res, byID, *batch, *parallel, *requestTimeout)
	if err != nil {
		return failf("verify", err)
	}
	counts := map[string]int{}
	for _, v := range verdicts {
		counts[v.Verdict]++
	}
	js, _ := json.MarshalIndent(struct {
		Backend  string         `json:"backend"`
		Seconds  float64        `json:"seconds"`
		CostUSD  float64        `json:"cost_usd"`
		Counts   map[string]int `json:"counts"`
		Verdicts []verdict      `json:"verdicts"`
	}{m.Name(), time.Since(start).Seconds(), usage.CostUSD, counts, verdicts}, "", "  ")
	if err := os.WriteFile(*outPath, js, 0o644); err != nil {
		return failf("verify", err)
	}
	fmt.Fprintf(out, "%d conflict(s): %d real, %d resolved in text, %d uncertain, %d artifact; about $%.2f, %.0fs\n",
		len(verdicts), counts["real"], counts["resolved"], counts["uncertain"], counts["artifact"], usage.CostUSD, time.Since(start).Seconds())
	return 0
}

type verdict struct {
	Conflict int    `json:"conflict"` // 1-based, as in the check output and packets
	Verdict  string `json:"verdict"`  // real | resolved | uncertain | artifact
	Class    string `json:"class,omitempty"`
	Reason   string `json:"reason"`
}

const verifyInstructions = `You judge conflicts that a checker found between software requirement statements.

Each statement was translated into rules automatically, and the
translations are often wrong. For each conflict you get the situation the
checker found, the rules that cannot all hold in it, and the statements the
rules came from. Decide whether the statements, as a careful engineer reads
them, really cannot all hold in that situation (or one like it that the
statements allow):

- real: the statements as written cannot all hold; an author would have to
  change one of them.
- resolved: the statements' own text says which one wins here (an
  exception, "unless", "regardless of", "per FR-x it does not apply"), and
  the rules dropped it.
- uncertain: the statements are genuinely ambiguous on this point; say what
  the question is.
- artifact: a rule misreads its statement, so the conflict is not in the
  specs. Name the class: over-broad assert (asserts more than the statement,
  or where it makes no claim), missing world fact (the situation cannot
  happen in reality), phase or assumption (the rule applies at a step or in a
  case the statement does not), asserts an input (it constrains what the
  step only reads), one value two names, wrong world fact (a proposed fact
  that is false), single statement (one statement's rules clash).

Judge the statements, not the code, and not which statement is better. A
conflict is real only if both statements, read as written, demand what the
rules demand in that situation. Do not call a conflict an artifact because
the specs could be read charitably: when two statements plainly demand
different outcomes for the same situation, it is real. Give a one-sentence
reason.
`

const verifySchema = `{"type":"object","additionalProperties":false,"required":["verdicts"],"properties":{"verdicts":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["conflict","verdict","class","reason"],"properties":{"conflict":{"type":"string"},"verdict":{"type":"string","enum":["real","resolved","uncertain","artifact"]},"class":{"type":"string"},"reason":{"type":"string"}}}}}}`

func verifyConflicts(ctx context.Context, m llm.Client, reg *ir.Registry, res *checker.Result, byID map[string]translate.Statement, batch, parallel int, timeout time.Duration) ([]verdict, llm.Usage, error) {
	render := func(i int, c checker.Conflict) string {
		var b strings.Builder
		fmt.Fprintf(&b, "conflict: c%d\nsituation: %s\n", i+1, strings.Join(situation(reg, c.Counterexample), ", "))
		shown := map[string]bool{}
		for _, id := range c.Rules {
			x := reg.Rule(id)
			if x == nil {
				fmt.Fprintf(&b, "rule %s: an operation's own constraint\n", id)
				continue
			}
			fmt.Fprintf(&b, "rule %s: %s\n", id, ruleText(x))
			sid := x.Statement()
			if s, ok := byID[sid]; ok && !shown[sid] {
				shown[sid] = true
				fmt.Fprintf(&b, "  from statement %s: %s\n", sid, strings.Join(strings.Fields(s.Prose), " "))
			} else if isProposedFact(id) && x.Prose != "" {
				fmt.Fprintf(&b, "  proposed by the tool, not a statement: %s\n", x.Prose)
			}
		}
		return b.String()
	}
	out := make([]verdict, len(res.Conflicts))
	for i := range out {
		out[i] = verdict{Conflict: i + 1, Verdict: "uncertain", Reason: "no verdict returned"}
	}
	var mu sync.Mutex
	var usage llm.Usage
	var firstErr error
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for lo := 0; lo < len(res.Conflicts); lo += batch {
		hi := min(lo+batch, len(res.Conflicts))
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var req strings.Builder
			for i := lo; i < hi; i++ {
				req.WriteString(render(i, res.Conflicts[i]))
				req.WriteString("\n")
			}
			var reply *llm.Reply
			var err error
			for attempt := 0; attempt < 2; attempt++ {
				cctx, cancel := context.WithTimeout(ctx, timeout)
				reply, err = m.Complete(cctx, llm.Request{System: verifyInstructions, Turns: []llm.Turn{{Role: "user", Text: req.String()}}, Schema: verifySchema})
				cancel()
				if err == nil {
					break
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			usage.Add(reply.Usage)
			var got struct {
				Verdicts []struct{ Conflict, Verdict, Class, Reason string } `json:"verdicts"`
			}
			if err := json.Unmarshal([]byte(reply.JSON), &got); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("verify reply: %w", err)
				}
				return
			}
			for _, v := range got.Verdicts {
				var k int
				if _, err := fmt.Sscanf(v.Conflict, "c%d", &k); err != nil || k < lo+1 || k > hi {
					continue
				}
				out[k-1] = verdict{Conflict: k, Verdict: v.Verdict, Class: v.Class, Reason: v.Reason}
			}
		}(lo, hi)
	}
	wg.Wait()
	return out, usage, firstErr
}
