package adjudicate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// Clause is one requirement inside a statement, as the edit check found it
// in the code.
type Clause struct {
	StatementID string `json:"statement_id"`
	Text        string `json:"text"`
	// Code is holds (the code does it), contradicted (the code does
	// otherwise), absent (no code for it) or unknown (not shown).
	Code string `json:"code"`
	Cite *Cite  `json:"cite,omitempty"`
}

const editSystem = `You check a drafted spec edit against the project's code before a
maintainer applies it.

A previous step read the code and decided which of the statements below the
code follows, which are stale (the code does otherwise) and which are
overbroad (true only in a narrower case). It drafted one edit. The verdict
is settled; your job is the edit.

1. Split every statement (followed, stale and overbroad alike) into its
   separate requirements: each SHALL/MUST clause, condition, value, name,
   path and scenario. For each, decide from the excerpts whether the code:
   holds (does it), contradicted (does otherwise), absent (has no code for
   it), or unknown (the excerpts do not show it). Cite a file, line and short
   exact quote for holds and contradicted.
2. Write the edit the maintainer should apply:
   - Change only what the code contradicts or lacks; keep every clause the
     code still does. Do not retire a statement or a whole spec while any of
     its clauses holds; rewrite it.
   - When the code ignores a condition the statement imposes, replace that
     condition; do not add a new condition beside it.
   - Do not write in behaviour the code does not have, even if another
     statement or the draft describes it.
   - Include contradicted clauses of the followed statements too.
   - Where the excerpts settle a value or a name, state it; do not ask the
     maintainer to check it. Name what to check only for unknown clauses.
   - Name each statement by its id and quote the words to change.
3. If the excerpts do not show the code for a clause the edit depends on,
   answer status need_code and list in read the files and line ranges to
   show (at most 4, each at most 150 lines). You can ask once.

changed: whether your edit differs in substance from the draft.`

const editSchema = `{"type":"object","additionalProperties":false,"required":["status","read","clauses","edit","changed","why"],"properties":{` +
	`"status":{"type":"string","enum":["ok","need_code"]},` +
	`"read":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["file","from","to"],"properties":{"file":{"type":"string"},"from":{"type":"integer"},"to":{"type":"integer"}}}},` +
	`"clauses":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["statement_id","text","code","file","line","quote"],"properties":{` +
	`"statement_id":{"type":"string"},"text":{"type":"string"},"code":{"type":"string","enum":["holds","contradicted","absent","unknown"]},` +
	`"file":{"type":"string"},"line":{"type":"integer"},"quote":{"type":"string"}}}},` +
	`"edit":{"type":"string"},"changed":{"type":"boolean"},"why":{"type":"string"}}}`

// CheckEdits runs the edit check on every superseded or overbroad verdict
// in vs, in place, and returns the usage. The verdicts themselves are not
// changed; each gets Edit and Clauses.
func CheckEdits(ctx context.Context, m llm.Client, stmts []translate.Statement, vs []Verdict, opt Options, progress func(Verdict)) llm.Usage {
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
	if opt.Parallel < 1 {
		opt.Parallel = 1
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var total llm.Usage
	sem := make(chan struct{}, opt.Parallel)
	for i := range vs {
		if vs[i].Kind != "superseded" && vs[i].Kind != "overbroad" || vs[i].Outcome == "unclear" {
			continue
		}
		wg.Add(1)
		go func(v *Verdict) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			u := checkEdit(ctx, m, byID, v, opt)
			mu.Lock()
			total.Add(u)
			if progress != nil {
				progress(*v)
			}
			mu.Unlock()
		}(&vs[i])
	}
	wg.Wait()
	return total
}

func checkEdit(ctx context.Context, m llm.Client, byID map[string]translate.Statement, v *Verdict, opt Options) llm.Usage {
	var usage llm.Usage
	var b strings.Builder
	role := func(id string) string {
		switch {
		case contains(v.Stale, id):
			return "stale"
		case contains(v.Overbroad, id):
			return "overbroad"
		case contains(v.Follows, id):
			return "followed"
		}
		return "not named"
	}
	for _, id := range v.StatementIDs {
		s, ok := byID[id]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "statement %s (%s)\n  source: %s\n  text: %s\n\n", id, role(id), llm.Data(s.Source), llm.Data(strings.Join(strings.Fields(s.Prose), " ")))
	}
	fmt.Fprintf(&b, "Verdict: %s. Reason: %s\n\nDrafted edit: %s\n\n", v.Kind, v.Reason, v.Action)
	header := b.String()

	// The code: the verdict's search terms again, and the lines around each
	// valid citation, so the clauses the verdict rested on are in view.
	var reads []LineRange
	for _, c := range v.Cites {
		if c.Valid {
			reads = append(reads, LineRange{c.File, c.Line - 20, c.Line + 30})
		}
	}
	ev, err := Gather(opt.Repo, v.Terms, opt.Evidence)
	if err != nil {
		v.EditNote = "code search failed: " + err.Error()
		return usage
	}
	for round := 1; round <= 2; round++ {
		user := header + "Code excerpts:\n\n" + RenderRanges(opt.Repo, mergeRanges(reads)) + ev.Render()
		cctx, cancel := context.WithTimeout(ctx, opt.Timeout)
		reply, err := m.Complete(cctx, llm.Request{System: editSystem, Schema: editSchema, Turns: []llm.Turn{{Role: "user", Text: user}}})
		cancel()
		if err != nil {
			v.EditNote = "model call failed: " + err.Error()
			return usage
		}
		usage.Add(reply.Usage)
		var got struct {
			Status  string      `json:"status"`
			Read    []LineRange `json:"read"`
			Clauses []struct {
				StatementID string `json:"statement_id"`
				Text        string `json:"text"`
				Code        string `json:"code"`
				File        string `json:"file"`
				Line        int    `json:"line"`
				Quote       string `json:"quote"`
			} `json:"clauses"`
			Edit    string `json:"edit"`
			Changed bool   `json:"changed"`
			Why     string `json:"why"`
		}
		if err := json.Unmarshal([]byte(reply.JSON), &got); err != nil {
			v.EditNote = "unreadable reply: " + err.Error()
			return usage
		}
		if got.Status == "need_code" && round == 1 && len(got.Read) > 0 {
			reads = append(reads, got.Read[:min(len(got.Read), 4)]...)
			continue
		}
		if got.Status == "need_code" {
			// Still short of code: keep the draft, and say what to check.
			v.EditNote = "edit not checked, code not shown: " + got.Why
			return usage
		}
		v.Clauses = nil
		for _, c := range got.Clauses {
			cl := Clause{StatementID: c.StatementID, Text: c.Text, Code: c.Code}
			if c.File != "" && c.Line > 0 {
				cite := Cite{File: c.File, Line: c.Line, Quote: c.Quote}
				cite.Valid = checkCite(opt.Repo, cite)
				cl.Cite = &cite
			}
			v.Clauses = append(v.Clauses, cl)
		}
		v.Edit, v.EditChanged, v.EditNote = got.Edit, got.Changed, got.Why
		return usage
	}
	return usage
}

// mergeRanges joins overlapping ranges of one file, so a line is shown once.
func mergeRanges(rs []LineRange) []LineRange {
	var out []LineRange
	for _, r := range rs {
		r.From = max(1, r.From)
		merged := false
		for i := range out {
			if out[i].File == r.File && r.From <= out[i].To+1 && r.To >= out[i].From-1 {
				out[i].From, out[i].To = min(out[i].From, r.From), max(out[i].To, r.To)
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, r)
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
