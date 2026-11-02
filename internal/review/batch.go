package review

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// A corpus too large for one prompt is reviewed by area, then across areas
// (docs/08 W5 step 2, D-045). Area reviews see statements in full. The
// cross-area review sees every statement at once, condensed to its title and
// normative sentences, so a contradiction between two areas is in one prompt
// without any retrieval step guessing which statements to pair. Planning is
// deterministic; only the reviews call the model.

// Batch is one review call: the statements it shows the model.
type Batch struct {
	Kind       string // "all", "area" or "condensed"
	Label      string
	Statements []translate.Statement
	Tokens     int // estimated prompt size
}

// Plan splits stmts into review calls of about budget tokens. A budget of 0,
// or a corpus that fits, gives one call. Otherwise area calls pack statements
// in order, so statements from one file stay together, and condensed calls
// cover the whole corpus; the condensed corpus is split only if it alone
// exceeds the budget, and Plan reports that cross-area pairs are then missed.
func Plan(stmts []translate.Statement, budget int) (batches []Batch, warning string) {
	total := 0
	for _, s := range stmts {
		total += Tokens(s)
	}
	if budget <= 0 || total <= budget {
		return []Batch{{Kind: "all", Label: "all statements", Statements: stmts, Tokens: total + Overhead}}, ""
	}
	batches = pack(stmts, budget, "area")
	var short []translate.Statement
	for _, s := range stmts {
		c := s
		c.Prose = Condense(s.Prose)
		short = append(short, c)
	}
	cond := pack(short, budget, "condensed")
	if len(cond) > 1 {
		warning = fmt.Sprintf("the condensed corpus needs %d calls; contradictions between statements in different condensed calls and different areas are not reviewed (raise --budget-tokens)", len(cond))
	}
	return append(batches, cond...), warning
}

func pack(stmts []translate.Statement, budget int, kind string) []Batch {
	var out []Batch
	var cur Batch
	flush := func() {
		if len(cur.Statements) > 0 {
			cur.Tokens += Overhead
			cur.Kind = kind
			cur.Label = fmt.Sprintf("%s %d: %s", kind, len(out)+1, span(cur.Statements))
			out = append(out, cur)
		}
		cur = Batch{}
	}
	for _, s := range stmts {
		t := Tokens(s)
		if len(cur.Statements) > 0 && cur.Tokens+t > budget {
			flush()
		}
		cur.Statements = append(cur.Statements, s)
		cur.Tokens += t
	}
	flush()
	return out
}

func span(stmts []translate.Statement) string {
	first, last := fileOf(stmts[0].Source), fileOf(stmts[len(stmts)-1].Source)
	if first == last {
		return fmt.Sprintf("%s (%d statements)", first, len(stmts))
	}
	return fmt.Sprintf("%s .. %s (%d statements)", first, last, len(stmts))
}

func fileOf(source string) string {
	if i := strings.IndexByte(source, '#'); i >= 0 {
		return source[:i]
	}
	return source
}

// Tokens estimates the prompt tokens one statement takes: about three bytes
// per token for ASCII (requirements carry code names and paths, which
// tokenize densely; four bytes per token ran 25% low on Project B, ISSUES
// 31), one token per other character (CJK text is denser).
func Tokens(s translate.Statement) int {
	n := 0
	for _, text := range []string{s.ID, s.Source, s.Prose} {
		for _, r := range text {
			if r < 128 {
				n++
			} else {
				n += 3
			}
		}
	}
	return n/3 + 10
}

// Overhead is the prompt every review call carries besides its statements:
// the instructions and the reply schema.
var Overhead = (len(systemPrompt) + len(outputSchema)) / 3

var normativeWord = regexp.MustCompile(`\b(MUST|SHALL|SHOULD|MAY|REQUIRED)\b|なければならない|てはならない`)

// Condense keeps a statement's first line and the sentences that carry an
// RFC 2119 keyword (or its Japanese form), and drops OpenSpec scenarios. A
// statement with no such sentence keeps its first two lines.
func Condense(prose string) string {
	lines := strings.Split(prose, "\n")
	out := []string{lines[0]}
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#### Scenario") {
			break
		}
		for _, sent := range splitSentences(t) {
			if normativeWord.MatchString(sent) {
				out = append(out, sent)
			}
		}
	}
	if len(out) == 1 && len(lines) > 1 {
		out = append(out, strings.TrimSpace(lines[1]))
	}
	return strings.Join(out, "\n")
}

func splitSentences(line string) []string {
	var out []string
	start := 0
	rs := []rune(line)
	for i, r := range rs {
		if r == '。' || (r == '.' && (i+1 == len(rs) || rs[i+1] == ' ')) {
			if s := strings.TrimSpace(string(rs[start : i+1])); s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
	}
	if s := strings.TrimSpace(string(rs[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

// Batches reviews each batch, at most parallel at a time, and merges the
// findings: a set of statements reported by several batches is kept once.
// A batch that fails is recorded in Failed and the others still count.
func Batches(ctx context.Context, m llm.Client, batches []Batch, facts []Fact, parallel int, progress func(i int, b Batch, r *Result, err error)) *Result {
	if parallel < 1 {
		parallel = 1
	}
	results := make([]*Result, len(batches))
	errs := make([]error, len(batches))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, parallel)
	for i, b := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i], errs[i] = Review(ctx, m, b.Statements, facts)
			if progress != nil {
				mu.Lock()
				progress(i, b, results[i], errs[i])
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	out := &Result{Backend: m.Name(), Batches: len(batches)}
	seen := map[string]bool{}
	for i, r := range results {
		if errs[i] != nil {
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", batches[i].Label, errs[i]))
			continue
		}
		if out.Model == "" {
			out.Model = r.Model
		}
		out.Usage.Add(r.Usage)
		out.Unknown = append(out.Unknown, r.Unknown...)
		for _, f := range r.Findings {
			ids := append([]string{}, f.StatementIDs...)
			sort.Strings(ids)
			key := strings.Join(ids, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			f.Batch = batches[i].Kind
			out.Findings = append(out.Findings, f)
		}
	}
	return out
}
