package adjudicate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// Candidate is a set of statements suspected not to hold together, with the
// situation the reviewer named.
type Candidate struct {
	StatementIDs []string `json:"statement_ids"`
	Situation    string   `json:"situation"`
}

// Cite is a claim about the code, checked against the file.
type Cite struct {
	File  string `json:"file"`
	Line  int    `json:"line"`
	Quote string `json:"quote"`
	Valid bool   `json:"valid"`
}

// Verdict is the decision on one candidate.
type Verdict struct {
	Candidate    int              `json:"candidate"` // 1-based
	StatementIDs []string         `json:"statement_ids"`
	Situation    string           `json:"situation,omitempty"`
	Dates        map[string]Dated `json:"dates,omitempty"`
	Terms        []string         `json:"terms"`
	Hits         map[string]int   `json:"hits,omitempty"`
	Rounds       int              `json:"rounds"`
	Kind         string           `json:"kind"` // what the model said: superseded | overbroad | compatible | resolved_in_text | not_implemented | unclear
	Follows      []string         `json:"follows,omitempty"`
	Stale        []string         `json:"stale,omitempty"`
	Overbroad    []string         `json:"overbroad,omitempty"`
	Outcome      string           `json:"outcome"` // newer_followed | older_followed | followed_undated | compatible | resolved_in_text | not_implemented | unclear
	Action       string           `json:"action,omitempty"`
	Reason       string           `json:"reason"`
	Cites        []Cite           `json:"cites,omitempty"`
	Note         string           `json:"note,omitempty"`
	// Edit is the spec edit after the edit check: the draft in Action,
	// checked clause by clause against the code (Clauses).
	Edit        string    `json:"edit,omitempty"`
	EditChanged bool      `json:"edit_changed,omitempty"`
	EditNote    string    `json:"edit_note,omitempty"`
	Clauses     []Clause  `json:"clauses,omitempty"`
	Usage       llm.Usage `json:"usage"`
}

// Options control a run.
type Options struct {
	Repo     string
	Evidence EvidenceOptions
	Rounds   int  // search rounds per candidate (default 2)
	NoCode   bool // ablation: decide from the statements alone
	NoDates  bool // ablation: do not show the model when each statement was written
	Timeout  time.Duration
	Parallel int // candidates decided at a time (default 1)
	// Ranges lets the verdict call ask to read file line ranges it names,
	// not only more search terms (experimental, after the 2026-10-27 freeze).
	Ranges bool
}

// LineRange is a span of a file the verdict call asked to read.
type LineRange struct {
	File string `json:"file"`
	From int    `json:"from"`
	To   int    `json:"to"`
}

const rangesAddendum = `

When the excerpts show a call or a file but not the code that decides the
question (a function body cut off, a handler not shown), choose
need_evidence and list in read the files and line ranges to show next
(at most 4, each at most 150 lines), as well as or instead of more_terms.`

var verdictSchemaRanges = strings.Replace(verdictSchema, `"more_terms":{"type":"array","items":{"type":"string"}},`,
	`"more_terms":{"type":"array","items":{"type":"string"}},"read":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["file","from","to"],"properties":{"file":{"type":"string"},"from":{"type":"integer"},"to":{"type":"integer"}}}},`, 1)

// RenderRanges prints the requested ranges of files in repo, each capped
// at 150 lines; paths outside the repository are skipped.
func RenderRanges(repo string, reads []LineRange) string {
	var b strings.Builder
	for _, r := range reads {
		if r.File == "" || strings.Contains(r.File, "..") || filepath.IsAbs(r.File) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(r.File)))
		if err != nil {
			continue
		}
		lines := strings.Split(string(src), "\n")
		from := max(1, r.From)
		to := min(len(lines), max(from, r.To), from+149)
		fmt.Fprintf(&b, "--- %s, lines %d-%d (requested)\n", r.File, from, to)
		for n := from; n <= to; n++ {
			l := lines[n-1]
			if len(l) > 300 {
				l = l[:300] + "…"
			}
			fmt.Fprintf(&b, "%6d  %s\n", n, l)
		}
		b.WriteString("\n")
	}
	return b.String()
}

const termsSystem = `You help find the code that implements software requirements.

Given a few requirement statements, list the terms most likely to appear
literally in the project's source code or tests where this behaviour is
implemented: identifiers (in the casing the code would use, e.g. snake_case
for Rust/Python, camelCase or PascalCase for Go/TypeScript/Java), config
keys, CLI flags, enum variants, status strings, error or log message text,
key bindings, constants. Prefer distinctive terms over common words. Give
4 to 12 terms.`

const termsSchema = `{"type":"object","additionalProperties":false,"required":["terms"],"properties":{"terms":{"type":"array","items":{"type":"string"}}}}`

const verdictSystem = `You decide suspected conflicts between software requirement statements
by reading the project's code.

You get statements a reviewer thinks cannot all hold, the situation the
reviewer named, when each statement's text was last written (and the
commit that wrote it), and excerpts from the code and tests found by
searching for terms. Decide what the code does now. Choose one kind:

- superseded: the code implements some statements (follows) and does the
  opposite of others (stale). List both by statement id. A statement that
  requires a feature the code does not have, while another statement says
  the feature is absent or removed, is stale.
- overbroad: the code is consistent with every statement, but some hold only
  in a narrower case than they state (the other statements narrow them).
  Put those in overbroad (not in follows) and the rest in follows; say in
  action what condition the overbroad statement lacks.
- compatible: the statements, read as written, can all hold, and the code
  does all of them; the reviewer was wrong.
- resolved_in_text: the statements' own text already says which applies.
- not_implemented: the code for this behaviour does not exist, so the code
  cannot decide.
- need_evidence: the excerpts do not show the code that decides it; give
  more_terms to search for.
- unclear: the code was found but does not decide it.

Rules:
- Every claim about the code needs a cite: a file and line from the
  excerpts and a short exact quote of that line (or of a line within two
  lines of it). Tests and comments that state the intended behaviour are
  evidence.
- Decide from what the code does, not from which statement is newer or
  better written. Dates are context only.
- Use only statement ids given to you.
- action: one sentence for the spec's maintainer (retire X; add condition C
  to Y; check the code of Z against Y).`

const verdictSchema = `{"type":"object","additionalProperties":false,"required":["kind","follows","stale","overbroad","more_terms","cites","action","reason"],"properties":{` +
	`"kind":{"type":"string","enum":["superseded","overbroad","compatible","resolved_in_text","not_implemented","need_evidence","unclear"]},` +
	`"follows":{"type":"array","items":{"type":"string"}},"stale":{"type":"array","items":{"type":"string"}},"overbroad":{"type":"array","items":{"type":"string"}},` +
	`"more_terms":{"type":"array","items":{"type":"string"}},` +
	`"cites":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["file","line","quote"],"properties":{"file":{"type":"string"},"line":{"type":"integer"},"quote":{"type":"string"}}}},` +
	`"action":{"type":"string"},"reason":{"type":"string"}}}`

const textOnlySystem = `You decide suspected conflicts between software requirement statements.

You get statements a reviewer thinks cannot all hold, the situation the
reviewer named, and when each statement's text was last written. Without
looking at any code, decide which kind applies:

- superseded: some statements replace others (list follows and stale).
- overbroad: some statements hold only in a narrower case than stated (list
  them as overbroad, the rest as follows).
- compatible: the statements can all hold as written.
- resolved_in_text: the statements' own text says which applies.
- unclear: you cannot tell.

Use only statement ids given to you. Leave cites and more_terms empty.
action: one sentence for the spec's maintainer.`

// Decide adjudicates each candidate.
func Decide(ctx context.Context, m llm.Client, stmts []translate.Statement, cands []Candidate, opt Options, progress func(Verdict)) ([]Verdict, llm.Usage) {
	if opt.Rounds == 0 {
		opt.Rounds = 2
	}
	if opt.Timeout == 0 {
		opt.Timeout = 10 * time.Minute
	}
	byID := map[string]translate.Statement{}
	var sources []string
	for _, s := range stmts {
		byID[s.ID] = s
		sources = append(sources, s.Source)
	}
	if opt.Evidence.Exclude == nil {
		opt.Evidence.Exclude = SpecDirs(sources)
	}
	dates := DateStatements(opt.Repo, stmts)
	if opt.Parallel < 1 {
		opt.Parallel = 1
	}
	out := make([]Verdict, len(cands))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, opt.Parallel)
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c Candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v := decideOne(ctx, m, byID, dates, c, opt)
			v.Candidate = i + 1
			mu.Lock()
			out[i] = v
			if progress != nil {
				progress(v)
			}
			mu.Unlock()
		}(i, c)
	}
	wg.Wait()
	var total llm.Usage
	for _, v := range out {
		total.Add(v.Usage)
	}
	return out, total
}

func decideOne(ctx context.Context, m llm.Client, byID map[string]translate.Statement, dates map[string]Dated, c Candidate, opt Options) Verdict {
	v := Verdict{StatementIDs: c.StatementIDs, Situation: c.Situation, Dates: map[string]Dated{}}
	var known []string
	var stmtText strings.Builder
	for _, id := range c.StatementIDs {
		s, ok := byID[id]
		if !ok {
			continue
		}
		known = append(known, id)
		d := dates[id]
		v.Dates[id] = d
		fmt.Fprintf(&stmtText, "statement %s\n  source: %s\n", id, s.Source)
		if !opt.NoDates && d.Date != "" {
			fmt.Fprintf(&stmtText, "  last written: %s", d.Date)
			if d.Subject != "" {
				fmt.Fprintf(&stmtText, " (commit %s: %s)", d.Commit, d.Subject)
			}
			stmtText.WriteString("\n")
		}
		fmt.Fprintf(&stmtText, "  text: %s\n\n", strings.Join(strings.Fields(s.Prose), " "))
	}
	if len(known) < 2 {
		v.Kind, v.Outcome, v.Reason = "unclear", "unclear", "fewer than two of the candidate's statements are known"
		return v
	}
	call := func(system, schema, user string) (string, error) {
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			cctx, cancel := context.WithTimeout(ctx, opt.Timeout)
			reply, err := m.Complete(cctx, llm.Request{System: system, Schema: schema, Turns: []llm.Turn{{Role: "user", Text: user}}})
			cancel()
			if err == nil {
				v.Usage.Add(reply.Usage)
				return reply.JSON, nil
			}
			lastErr = err
		}
		return "", lastErr
	}
	header := fmt.Sprintf("Situation named by the reviewer: %s\n\n%s", c.Situation, stmtText.String())

	if opt.NoCode {
		js, err := call(textOnlySystem, verdictSchema, header)
		if err != nil {
			v.Kind, v.Outcome, v.Reason = "unclear", "unclear", "model call failed: "+err.Error()
			return v
		}
		v.Rounds = 1
		finish(&v, js, known, opt.Repo, false)
		return v
	}

	var terms []string
	for _, id := range known {
		terms = append(terms, QuotedNames(byID[id].Prose)...)
	}
	if js, err := call(termsSystem, termsSchema, stmtText.String()); err == nil {
		var got struct{ Terms []string }
		if json.Unmarshal([]byte(js), &got) == nil {
			terms = append(terms, got.Terms...)
		}
	}
	terms = dedupe(terms)
	if len(terms) > 20 {
		terms = terms[:20]
	}
	retried := false
	var reads []LineRange
	for round := 1; round <= opt.Rounds; round++ {
		v.Rounds = round
		ev, err := Gather(opt.Repo, terms, opt.Evidence)
		if err != nil {
			v.Kind, v.Outcome, v.Reason = "unclear", "unclear", "code search failed: "+err.Error()
			return v
		}
		v.Terms, v.Hits = terms, ev.Hits
		system, schema, shown := verdictSystem, verdictSchema, ev.Render()
		if opt.Ranges {
			system, schema = verdictSystem+rangesAddendum, verdictSchemaRanges
			shown = RenderRanges(opt.Repo, reads) + shown
		}
		js, err := call(system, schema, header+"Code excerpts:\n\n"+shown)
		if err != nil {
			v.Kind, v.Outcome, v.Reason = "unclear", "unclear", "model call failed: "+err.Error()
			return v
		}
		var peek struct {
			Kind      string      `json:"kind"`
			MoreTerms []string    `json:"more_terms"`
			Read      []LineRange `json:"read"`
		}
		_ = json.Unmarshal([]byte(js), &peek)
		if peek.Kind == "need_evidence" && round < opt.Rounds && len(peek.MoreTerms)+len(peek.Read) > 0 {
			terms = dedupe(append(terms, peek.MoreTerms...))
			reads = append(reads, peek.Read[:min(len(peek.Read), 4)]...)
			continue
		}
		finish(&v, js, known, opt.Repo, true)
		if strings.Contains(v.Note, "named no stale or overbroad") && !retried {
			// One corrective turn: the reply chose a decisive kind but put
			// every statement in follows.
			retried = true
			header += "Your previous answer chose " + v.Kind + " but listed no statement as stale or overbroad. " +
				"Put the statements the code contradicts in stale, and those that hold only in a narrower case in overbroad.\n\n"
			v.Note = ""
			round--
			continue
		}
		return v
	}
	return v
}

// finish reads the model's reply into v, checks its citations against the
// files, and derives the outcome from the statements' dates.
func finish(v *Verdict, js string, known []string, repo string, needCites bool) {
	var got struct {
		Kind      string   `json:"kind"`
		Follows   []string `json:"follows"`
		Stale     []string `json:"stale"`
		Overbroad []string `json:"overbroad"`
		Cites     []Cite   `json:"cites"`
		Action    string   `json:"action"`
		Reason    string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		v.Kind, v.Outcome, v.Reason = "unclear", "unclear", "unreadable reply: "+err.Error()
		return
	}
	in := map[string]bool{}
	for _, id := range known {
		in[id] = true
	}
	keep := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if in[id] {
				out = append(out, id)
			}
		}
		return out
	}
	v.Kind, v.Action, v.Reason = got.Kind, got.Action, got.Reason
	v.Follows, v.Stale, v.Overbroad = keep(got.Follows), keep(got.Stale), keep(got.Overbroad)
	if (v.Kind == "superseded" || v.Kind == "overbroad") && len(v.Follows) == 0 {
		// The statements not named stale or overbroad are the ones followed.
		lost := map[string]bool{}
		for _, id := range append(append([]string{}, v.Stale...), v.Overbroad...) {
			lost[id] = true
		}
		for _, id := range known {
			if !lost[id] {
				v.Follows = append(v.Follows, id)
			}
		}
	}
	valid := 0
	for _, c := range got.Cites {
		c.Valid = checkCite(repo, c)
		if c.Valid {
			valid++
		}
		v.Cites = append(v.Cites, c)
	}
	if v.Kind == "need_evidence" {
		v.Kind = "unclear"
		v.Note = "still needed evidence after the last search round"
	}
	if (v.Kind == "superseded" || v.Kind == "overbroad") && len(v.Stale)+len(v.Overbroad) == 0 {
		v.Outcome = "unclear"
		v.Note = strings.TrimSpace(v.Note + " the verdict named no stale or overbroad statement")
		return
	}
	decisive := v.Kind == "superseded" || v.Kind == "overbroad" || v.Kind == "compatible"
	if needCites && decisive && valid == 0 {
		v.Outcome = "unclear"
		v.Note = strings.TrimSpace(v.Note + " no citation matched the files; the " + v.Kind + " verdict is not used")
		return
	}
	v.Outcome = outcome(v)
}

// outcome turns a superseded or overbroad verdict into whether the code
// follows the newer statements, by the statements' dates. Only stale
// statements are compared: an overbroad statement is consistent with the
// code in the case it narrows to, so its date says nothing about which way
// the code went, and a verdict with only overbroad statements is "overbroad".
func outcome(v *Verdict) string {
	switch v.Kind {
	case "superseded", "overbroad":
	default:
		return v.Kind
	}
	losers := v.Stale
	if len(losers) == 0 {
		if len(v.Overbroad) > 0 {
			return "overbroad"
		}
		return "unclear"
	}
	if len(v.Follows) == 0 {
		return "unclear"
	}
	newestWinner := ""
	for _, id := range v.Follows {
		d := v.Dates[id].Date
		if d == "" {
			return "followed_undated"
		}
		if d > newestWinner {
			newestWinner = d
		}
	}
	for _, id := range losers {
		d := v.Dates[id].Date
		if d == "" {
			return "followed_undated"
		}
		if d > newestWinner {
			return "older_followed"
		}
	}
	return "newer_followed"
}

// checkCite reports whether quote appears on the cited line of file or
// within two lines of it, ignoring case and runs of white space.
func checkCite(repo string, c Cite) bool {
	if c.File == "" || c.Line < 1 || strings.Contains(c.File, "..") {
		return false
	}
	src, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(c.File)))
	if err != nil {
		return false
	}
	lines := strings.Split(string(src), "\n")
	q := norm(strings.Trim(c.Quote, "`\"' "))
	if len(q) < 4 {
		return false
	}
	for n := max(1, c.Line-2); n <= min(len(lines), c.Line+2); n++ {
		if strings.Contains(norm(lines[n-1]), q) {
			return true
		}
	}
	// A quote spanning lines: join the window.
	lo, hi := max(1, c.Line-2), min(len(lines), c.Line+4)
	if lo <= hi && strings.Contains(norm(strings.Join(lines[lo-1:hi], " ")), q) {
		return true
	}
	return false
}

func norm(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Counts tallies outcomes.
func Counts(vs []Verdict) map[string]int {
	out := map[string]int{}
	for _, v := range vs {
		out[v.Outcome]++
	}
	return out
}

// SortedKeys returns m's keys in order.
func SortedKeys(m map[string]int) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
