package formalize

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/provasign/stele/internal/lesson"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// Clause coverage and scope. A translated statement's rules may say less
// than its text (a condition dropped, "in the exact order" read as
// "applied", a specific message read as "non-empty") or more (a "short-
// lived" token read as "under one hour", an optional input made mandatory).
// Such rules compile and never contradict the statement, so no other check
// sees them: a weaker rule hides the conflicts the statement should raise,
// a broader one invents conflicts it should not. Of 59 translated
// statements graded (model-settled, 11 verdicts checked by the owner), 24
// were faithful, 31 weaker and 4 broader. The guidance below follows the
// owner where the owner overturned the model.
//
// After translation, the model is asked once per batch to split each
// statement into its clauses, say for each whether some rule fails whenever
// the system breaks it, and name any rule that forbids what the text allows.
// Statements with an uncovered clause or a broader rule are translated once
// more with their current rules and the problem named; what stays uncovered
// makes the statement partial, and a rule still broader is named in its
// reason, so the owner sees where the rules and the text part.

// Clause is one checkable part of a statement and whether its rules cover it.
type Clause struct {
	Text   string   `json:"text"`
	Status string   `json:"status"` // covered, uncovered, not_checkable
	Rules  []string `json:"rules,omitempty"`
}

// Clause statuses.
const (
	Covered      = "covered"
	Uncovered    = "uncovered"
	NotCheckable = "not_checkable"
)

// Excess is a rule that forbids something its statement allows.
type Excess struct {
	Rules []string `json:"rules"`
	What  string   `json:"what"` // what it forbids that the text allows
}

// StatementCoverage is the clause coverage and scope of one statement's rules.
type StatementCoverage struct {
	StatementID string   `json:"statement_id"`
	Clauses     []Clause `json:"clauses"`
	Broader     []Excess `json:"broader,omitempty"`
	// Retranslated is set when the statement was translated again because
	// of a finding; Clauses and Broader then describe the new rules.
	Retranslated bool `json:"retranslated,omitempty"`
}

// uncovered lists the clauses with no rule.
func (sc StatementCoverage) uncovered() []string {
	var out []string
	for _, c := range sc.Clauses {
		if c.Status == Uncovered {
			out = append(out, c.Text)
		}
	}
	return out
}

// broader describes the rules that go beyond the text.
func (sc StatementCoverage) broader() []string {
	var out []string
	for _, b := range sc.Broader {
		out = append(out, strings.Join(b.Rules, ", ")+": "+b.What)
	}
	return out
}

// Faithful reports whether every checkable clause is covered and no rule
// goes beyond the text.
func (sc StatementCoverage) Faithful() bool {
	return len(sc.uncovered()) == 0 && len(sc.Broader) == 0
}

// CoverStats summarizes a run's clause coverage check.
type CoverStats struct {
	Statements   int `json:"statements"`    // checked: with at least one rule
	Clauses      int `json:"clauses"`       // in the final check
	Covered      int `json:"covered"`       // clauses
	Uncovered    int `json:"uncovered"`     // clauses, after retranslation
	NotCheckable int `json:"not_checkable"` // clauses
	Broader      int `json:"broader"`       // statements with a rule beyond the text, after retranslation
	Retranslated int `json:"retranslated"`  // statements sent back with a finding named
	Recovered    int `json:"recovered"`     // of those, faithful afterwards
	Errors       int `json:"errors,omitempty"`
}

const coverPrompt = `You check whether rules say what a requirement says, no less and no more. For each statement you get its text, the rules translated from it, and the meaning of the facts those rules read. Facts are named by the translator: judge a rule by what its facts mean, never by whether their names repeat the text's words.

1. Split the text into clauses: each distinct thing it requires, forbids or permits; each condition, exception or restriction ("when", "unless", "except", "only", "before", "after", "for its entire lifetime"); each quantity, default, limit or exact value; each ordering or completeness claim ("all", "in the listed order", "including duplicates"); each named content ("the message names the missing field"). Quote each clause from the text, in order. Do not split finer than what one rule could check.

2. For each clause decide:
- covered: some listed rule fails in every situation where the system breaks this clause. Name those rules.
- uncovered: the system can break this clause while every listed rule holds.
- not_checkable: a rationale ("so that users can...", "rather than X" naming a design alternative), an example ("e.g."), a cross-reference, a judgment no check decides ("clear", "intuitive"), or the form of a value ("as an absolute timestamp").

How to judge coverage:
- A fact whose meaning is the clause's content covers it. "Order.receiptSent" covers "the system MUST email a receipt"; "Import.hasUnknownColumn" with the import failing covers "unknown columns are rejected"; "Cart.savedItemsCleared" covers "saved items, coupons and the draft address are removed" when the fact's meaning includes them. Do not ask for a rule that restates the text in other words, or one rule per item of a list the fact already denotes.
- A capability ("MUST support", "MUST accept", "MUST allow", "users can") is covered by a permission with effect possible or allow on the step that exercises it, in the situation the text names. The input it takes ("look up an order by its number", "accept a coupon code as input") is part of the capability, not a clause of its own.
- A clause that allows something or says two things may coincide ("names may overlap", "the list may be empty", "no limit on the count") is a clause like any other: it is covered by a rule that makes it possible (a possible permission, or a rule stating the overlap is accepted), and uncovered otherwise. If a rule forbids it, list that rule under broader.
- A fact does not cover a clause that asks for more than the fact states: a specific content ("the error names the missing field") against "an error is shown" or "the message is non-empty"; an order of steps against "the steps were applied"; a placement ("in the toolbar") that no rule mentions; a value tied to another ("the total equals the sum of the lines") against "the total is set"; an update on repeat ("re-running the import updates changed records") against "the records are saved"; a mechanism the text mandates over an alternative ("send through the queue instead of calling the service directly") against a rule that names only the outcome; an output that must contain something ("the report lists archived orders when asked") against a rule that only permits producing it.
- Adjectives such as "clear" or "helpful" are judgments, but what they qualify often is not: "rejected with a clear error message" asks for an error message, which is a clause.
- Check scope. A clause about every member of a kind ("explicit properties override role properties") is uncovered when the rules check only some members (only the colour). A clause asking for exactly a set ("only the main tags are kept") is uncovered by a rule that checks one direction (the result is a subset). A clause with a condition is uncovered when a rule's condition is narrower ("when the program cannot be run" against "when it is missing or lacks permission").
- Read each rule as Stele's language defines it. instances(E) is every other instance of E, never self: all(t in instances(Task): t.id != self.id) says no two tasks share an ID, so it covers a uniqueness clause and forbids nothing the text allows.
- A rule's condition or step is fine when it names where the clause naturally applies: a shipped order for its tracking number, a record checked when it is created, a value checked once it exists. Do not ask a rule to cover moments the text does not mention. A condition leaves the clause uncovered only when it excludes a case the text names (the text says "for every order" and the rule checks only paid ones).

A statement may come with no rules, when the translator read it as having nothing checkable: then each clause is uncovered (the system can break it) or not_checkable, as above.

3. Under broader, list each rule that forbids something the text allows: a limit, value or condition the text does not state ("recent" read as "within 24 hours"), something the text makes optional made mandatory, or a ban on an outcome for every cause where the text bans it for one cause ("never fail because of the item count" read as "never fail unless the payment fails"). Say in a few words what it forbids that the text allows. Leave the list empty when no rule goes beyond the text.

Reply for every requested statement.`

const coverSchema = `{"type":"object","additionalProperties":false,"required":["statements"],"properties":{"statements":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["statement_id","clauses","broader"],"properties":{"statement_id":{"type":"string"},"clauses":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["text","status","rules"],"properties":{"text":{"type":"string"},"status":{"type":"string","enum":["covered","uncovered","not_checkable"]},"rules":{"type":"array","items":{"type":"string"}}}}},"broader":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["rules","what"],"properties":{"rules":{"type":"array","items":{"type":"string"}},"what":{"type":"string"}}}}}}}}}`

// JudgeItem is one statement for the coverage judge: its text, its rules
// as the owner reads them, and the meaning of the facts they read.
type JudgeItem struct {
	ID    string
	Prose string
	Rules []JudgeRule
	Facts map[string]string // attribute -> meaning
	// Guidance is the owner's reading for this project (lessons, D-063),
	// which comes before the judge's general guidance.
	Guidance []string
}

// JudgeRule is one rule of a JudgeItem.
type JudgeRule struct {
	ID   string
	Text string
}

// JudgeReport is what Judge returns besides the judgments.
type JudgeReport struct {
	Usage  llm.Usage
	Errors []string
	Failed int // statements with no usable judgment
}

// Judge asks m, in batches of batch statements with up to parallel requests
// at once, which clause of each statement its rules cover and which rules
// go beyond it. A statement whose reply is missing or malformed is reported
// and left out: a failed check never marks a clause. The error is returned
// only when the run must stop (cancellation or the call budget).
func Judge(ctx context.Context, m llm.Client, items []JudgeItem, batch, parallel int) ([]StatementCoverage, JudgeReport, error) {
	var rep JudgeReport
	if len(items) == 0 {
		return nil, rep, nil
	}
	if batch <= 0 {
		batch = 20
	}
	parallel = max(1, parallel)
	type part struct {
		items   []JudgeItem
		aliases map[string]string            // s1 -> statement ID
		rules   map[string]map[string]string // statement ID -> r1 -> rule ID
		reply   *llm.Reply
		err     error
	}
	var parts []*part
	for i := 0; i < len(items); i += batch {
		parts = append(parts, &part{items: items[i:min(i+batch, len(items))]})
	}
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, p := range parts {
		p.aliases, p.rules = map[string]string{}, map[string]map[string]string{}
		var req strings.Builder
		req.WriteString("For each statement, list its clauses, say which rules cover each, and name any rule broader than the text.\n\n")
		for k, it := range p.items {
			alias := fmt.Sprintf("s%d", k+1)
			p.aliases[alias] = it.ID
			p.rules[it.ID] = map[string]string{}
			fmt.Fprintf(&req, "statement_id: %s\ntext: %s\nrules:\n", alias, strings.Join(strings.Fields(it.Prose), " "))
			for j, r := range it.Rules {
				short := fmt.Sprintf("r%d", j+1)
				p.rules[it.ID][short] = r.ID
				fmt.Fprintf(&req, "  %s: %s\n", short, r.Text)
			}
			if len(it.Guidance) > 0 {
				req.WriteString("the owner's reading for this project (follow it where it applies, before the general guidance):\n")
				for _, g := range it.Guidance {
					fmt.Fprintf(&req, "  - %s\n", g)
				}
			}
			if len(it.Facts) > 0 {
				names := make([]string, 0, len(it.Facts))
				for q := range it.Facts {
					names = append(names, q)
				}
				sort.Strings(names)
				req.WriteString("facts:\n")
				for _, q := range names {
					fmt.Fprintf(&req, "  %s: %s\n", q, it.Facts[q])
				}
			}
			req.WriteString("\n")
		}
		wg.Add(1)
		go func(p *part, text string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			p.reply, p.err = m.Complete(ctx, llm.Request{System: coverPrompt, Turns: []llm.Turn{{Role: "user", Text: text}}, Schema: coverSchema})
		}(p, req.String())
	}
	wg.Wait()
	var out []StatementCoverage
	for _, p := range parts {
		if p.err == nil && p.reply == nil {
			p.err = fmt.Errorf("backend returned no reply")
		}
		if p.err != nil {
			if j, ok := m.(*callJournal); (ok && j.stopErr() != nil) || ctx.Err() != nil {
				return nil, rep, fmt.Errorf("cover: %w", p.err)
			}
			rep.Failed += len(p.items)
			rep.Errors = append(rep.Errors, fmt.Sprintf("cover: request for %d statement(s) failed: %v", len(p.items), p.err))
			continue
		}
		rep.Usage.Add(p.reply.Usage)
		var reply struct {
			Statements []StatementCoverage `json:"statements"`
		}
		if err := json.Unmarshal([]byte(p.reply.JSON), &reply); err != nil {
			rep.Failed += len(p.items)
			rep.Errors = append(rep.Errors, fmt.Sprintf("cover: reply for %d statement(s) did not parse: %v", len(p.items), err))
			continue
		}
		seen := map[string]bool{}
		resolve := func(id string, short []string) []string {
			var full []string
			for _, r := range short {
				if f, ok := p.rules[id][r]; ok {
					full = append(full, f)
				} else if slices.Contains(slices.Collect(mapValues(p.rules[id])), r) {
					full = append(full, r)
				}
			}
			return full
		}
		for _, sc := range reply.Statements {
			id, ok := p.aliases[sc.StatementID]
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			sc.StatementID = id
			for i := range sc.Clauses {
				c := &sc.Clauses[i]
				c.Text = strings.Join(strings.Fields(c.Text), " ")
				c.Rules = resolve(id, c.Rules)
				if c.Status == Covered && len(c.Rules) == 0 {
					c.Status = Uncovered // covered by nothing is not covered
				}
			}
			var broader []Excess
			for _, b := range sc.Broader {
				if b.Rules = resolve(id, b.Rules); len(b.Rules) > 0 {
					b.What = strings.Join(strings.Fields(b.What), " ")
					broader = append(broader, b)
				}
			}
			sc.Broader = broader
			out = append(out, sc)
		}
		for _, it := range p.items {
			if !seen[it.ID] {
				rep.Failed++
				rep.Errors = append(rep.Errors, fmt.Sprintf("cover: no reply for %s", it.ID))
			}
		}
	}
	return out, rep, nil
}

func mapValues(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
}

// cover checks clause coverage and scope for every statement with a rule,
// and for every statement read as having nothing checkable at all, which
// may be a translation the model skipped ("the requested rule translation
// is not provided in this response", 2026-11-03); translates those with a
// finding once more, checks them again and records the result in st.
func cover(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options, dir string) error {
	withRules := func(ids []string) []translate.Statement {
		has, other := map[string]bool{}, map[string]bool{}
		for _, c := range st.Cands {
			switch {
			case c.Outcome == "rule" && c.Error == "":
				has[c.StatementID] = true
			case c.Outcome != Informal:
				other[c.StatementID] = true
			}
		}
		for _, c := range st.Cands {
			if c.Outcome == Informal && !other[c.StatementID] {
				has[c.StatementID] = true // every reading informal: judged too
			}
		}
		var out []translate.Statement
		for _, s := range stmts {
			if has[s.ID] && (ids == nil || slices.Contains(ids, s.ID)) {
				out = append(out, s)
			}
		}
		return out
	}
	first := withRules(nil)
	st.CoverStats = CoverStats{Statements: len(first)}
	fmt.Fprintf(opt.Log, "cover: checking the clauses and scope of %d statement(s) with rules or read as informal ...\n", len(first))
	cov, err := checkCoverage(ctx, m, first, st, opt)
	if err != nil {
		return err
	}
	var back []string
	for _, sc := range cov {
		if !sc.Faithful() {
			back = append(back, sc.StatementID)
		}
	}
	fmt.Fprintf(opt.Log, "cover: %d statement(s) have an uncovered clause or a rule beyond the text\n", len(back))
	if len(back) > 0 && opt.CoverRounds >= 0 {
		st.CoverStats.Retranslated = len(back)
		before := map[string][]translate.Candidate{}
		for _, c := range st.Cands {
			if slices.Contains(back, c.StatementID) {
				before[c.StatementID] = append(before[c.StatementID], c)
			}
		}
		if err := retranslateFindings(ctx, m, stmts, back, cov, st, opt, dir); err != nil {
			return err
		}
		again, err := checkCoverage(ctx, m, withRules(back), st, opt)
		if err != nil {
			return err
		}
		for i, sc := range again {
			// A reread the check finds worse than the reading it replaces
			// (fewer clauses covered, or as many with more findings) is
			// undone: the earlier rules and their coverage stand.
			if k := slices.IndexFunc(cov, func(x StatementCoverage) bool { return x.StatementID == sc.StatementID }); k >= 0 && worse(sc, cov[k]) && hasRule(before[sc.StatementID]) {
				id := sc.StatementID
				st.Cands = slices.DeleteFunc(st.Cands, func(c translate.Candidate) bool { return c.StatementID == id })
				st.Cands = append(st.Cands, before[id]...)
				sortByStatement(st.Cands, stmts)
				fmt.Fprintf(opt.Log, "cover: %s keeps its earlier rules: the new reading covers less\n", id)
				again[i] = cov[k]
				sc = cov[k]
			}
			sc.Retranslated = true
			if sc.Faithful() {
				st.CoverStats.Recovered++
			}
			k := slices.IndexFunc(cov, func(x StatementCoverage) bool { return x.StatementID == sc.StatementID })
			if k < 0 {
				cov = append(cov, sc)
			} else {
				cov[k] = sc
			}
		}
		fmt.Fprintf(opt.Log, "cover: %d of %d retranslated statement(s) now faithful\n", st.CoverStats.Recovered, len(back))
	}
	st.Coverage = cov
	st.Uncovered, st.Exceeds = map[string][]string{}, map[string][]string{}
	for _, sc := range cov {
		for _, c := range sc.Clauses {
			st.CoverStats.Clauses++
			switch c.Status {
			case Covered:
				st.CoverStats.Covered++
			case Uncovered:
				st.CoverStats.Uncovered++
			default:
				st.CoverStats.NotCheckable++
			}
		}
		if u := sc.uncovered(); len(u) > 0 {
			st.Uncovered[sc.StatementID] = u
		}
		if b := sc.broader(); len(b) > 0 {
			st.Exceeds[sc.StatementID] = b
			st.CoverStats.Broader++
		}
	}
	return nil
}

// describeCandidate renders a rule the way the owner sees it in a
// fidelity packet: kind, step or operation, subject, condition, assertion.
func describeCandidate(c translate.Candidate) string {
	head := c.Kind
	if c.On != "" {
		head += " on " + c.On
	}
	if c.Operation != "" {
		head += " " + c.Effect + " " + c.Operation
	}
	if c.Subject != "" {
		head += " (self: " + c.Subject + ")"
	}
	if c.When != "" {
		head += " when `" + c.When + "`"
	}
	if c.Assert != "" {
		head += " assert `" + c.Assert + "`"
	}
	if c.Assume {
		head += " (an assumption about the situation)"
	}
	return head
}

// checkCoverage judges the statements' current rules.
func checkCoverage(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options) ([]StatementCoverage, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	reg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, st.Proposal)
	if err != nil {
		return nil, fmt.Errorf("cover: %w", err)
	}
	facts := map[string]map[string]string{} // statement -> attribute -> meaning
	for _, r := range parseRules(reg, st.Cands) {
		for _, a := range r.reads {
			if a.Meaning == "" {
				continue
			}
			if facts[r.cand.StatementID] == nil {
				facts[r.cand.StatementID] = map[string]string{}
			}
			facts[r.cand.StatementID][a.QName()] = a.Meaning
		}
	}
	rulesOf := map[string][]JudgeRule{}
	for _, c := range st.Cands {
		if c.Outcome == "rule" && c.Error == "" {
			rulesOf[c.StatementID] = append(rulesOf[c.StatementID], JudgeRule{ID: c.RuleID, Text: describeCandidate(c)})
		}
	}
	var items []JudgeItem
	for _, s := range stmts {
		it := JudgeItem{ID: s.ID, Prose: s.Prose, Rules: rulesOf[s.ID], Facts: facts[s.ID]}
		for _, l := range lesson.Relevant(opt.Lessons, s.ID, translate.TextHash(s.Prose), s.Prose, 8) {
			if lesson.Guidance(l) {
				it.Guidance = append(it.Guidance, l.Says)
			}
		}
		items = append(items, it)
	}
	out, rep, err := Judge(ctx, m, items, opt.Batch, opt.Parallel)
	st.Usage.Add(rep.Usage)
	st.Errors = append(st.Errors, rep.Errors...)
	st.CoverStats.Errors += rep.Failed
	return out, err
}

// retranslateFindings translates the statements in ids again, each with its
// current rules and the judge's findings named, so the model can keep what
// is right and add or narrow the rest.
func retranslateFindings(ctx context.Context, m llm.Client, stmts []translate.Statement, ids []string, cov []StatementCoverage, st *state, opt Options, dir string) error {
	old := map[string][]translate.Candidate{}
	for _, c := range st.Cands {
		if slices.Contains(ids, c.StatementID) {
			old[c.StatementID] = append(old[c.StatementID], c)
		}
	}
	note := func(id string) string {
		k := slices.IndexFunc(cov, func(x StatementCoverage) bool { return x.StatementID == id })
		var b strings.Builder
		b.WriteString("the current rules are:")
		for _, c := range old[id] {
			if c.Outcome == "rule" && c.Error == "" {
				fmt.Fprintf(&b, " [%s]", describeCandidate(c))
			}
		}
		b.WriteString(".")
		if u := cov[k].uncovered(); len(u) > 0 {
			var quoted []string
			for _, x := range u {
				quoted = append(quoted, fmt.Sprintf("%q", x))
			}
			b.WriteString(" They leave these clauses of the text uncovered (the system can break them while every rule holds): " + strings.Join(quoted, "; ") + ".")
		}
		if x := cov[k].broader(); len(x) > 0 {
			b.WriteString(" These rules forbid what the text allows: " + strings.Join(x, "; ") + ".")
		}
		b.WriteString(" Keep the rules that are right. Add a rule that fails when each uncovered clause is broken, or return that clause as informal or needs_vocabulary with a rationale; narrow each rule that goes beyond the text to what the text says. " + keepFacts + ". Return every part of this statement")
		return b.String()
	}
	notes := map[string]string{}
	broader := map[string]bool{} // rules the check found beyond their text
	for _, id := range ids {
		notes[id] = note(id)
		if k := slices.IndexFunc(cov, func(x StatementCoverage) bool { return x.StatementID == id }); k >= 0 {
			for _, b := range cov[k].Broader {
				for _, r := range b.Rules {
					broader[r] = true
				}
			}
		}
	}
	fmt.Fprintf(opt.Log, "cover: translating %d statement(s) again with their findings named ...\n", len(ids))
	return retranslate(ctx, m, stmts, ids, notes, st, opt, dir, broader)
}

// retranslate translates the statements in ids again, each with its note.
// In compact mode the statements go through the compact repair channel;
// otherwise through the translation step with the note. A statement whose
// new translation fails keeps its old rules. With broader set (a coverage
// reread), so does one whose new translation has no rule where the old had
// one, less the old rules found beyond their text: a coverage finding asks
// for more of the text, and an answer of "informal" or "the vocabulary
// already says it" loses what was checkable.
func retranslate(ctx context.Context, m llm.Client, stmts []translate.Statement, ids []string, notes map[string]string, st *state, opt Options, dir string, broader map[string]bool) error {
	old := map[string][]translate.Candidate{}
	var kept []translate.Candidate
	for _, c := range st.Cands {
		if slices.Contains(ids, c.StatementID) {
			old[c.StatementID] = append(old[c.StatementID], c)
		} else {
			kept = append(kept, c)
		}
	}
	restore := func(id string, fresh []translate.Candidate) ([]translate.Candidate, bool) {
		if broader == nil || hasRule(fresh) || !hasRule(old[id]) {
			return nil, false
		}
		var keep []translate.Candidate
		for _, c := range old[id] {
			if !broader[c.RuleID] {
				keep = append(keep, c)
			}
		}
		return keep, hasRule(keep)
	}
	order := map[string]int{}
	for i, s := range stmts {
		order[s.ID] = i
	}
	if opt.Compact {
		// Keep the earlier rules in the saved state until the new reading
		// is accepted: compactPass saves as it goes, and a run stopped by its
		// budget or deadline must not lose these statements (restoreRereads).
		if st.Rereading == nil {
			st.Rereading = map[string][]translate.Candidate{}
		}
		for _, id := range ids {
			st.Rereading[id] = old[id]
		}
		st.Cands = kept
		if st.CompactErrors == nil {
			st.CompactErrors = map[string]string{} // a resumed run with no earlier errors saved none
		}
		if st.CompactDeferred == nil {
			st.CompactDeferred = map[string]string{}
		}
		for _, id := range ids {
			st.CompactErrors[id] = notes[id]
			delete(st.CompactDeferred, id)
		}
		if err := compactPass(ctx, m, stmts, st, opt, dir); err != nil {
			return err
		}
		for _, id := range ids {
			if _, deferred := st.CompactDeferred[id]; deferred {
				delete(st.CompactDeferred, id)
				delete(st.CompactErrors, id)
				st.Cands = append(st.Cands, old[id]...)
			} else if keep, ok := restore(id, candidatesOf(st.Cands, id)); ok {
				st.Cands = slices.DeleteFunc(st.Cands, func(c translate.Candidate) bool { return c.StatementID == id })
				st.Cands = append(st.Cands, keep...)
				fmt.Fprintf(opt.Log, "cover: %s kept its rules within the text: the new reading had none\n", id)
			}
			delete(st.Rereading, id)
		}
		sort.SliceStable(st.Cands, func(i, j int) bool { return order[st.Cands[i].StatementID] < order[st.Cands[j].StatementID] })
		return nil
	}
	files, err := writeVocabulary(dir, st.Proposal, opt)
	if err != nil {
		return err
	}
	var again []translate.Statement
	for _, s := range stmts {
		if slices.Contains(ids, s.ID) {
			s.Notes = append(slices.Clone(s.Notes), notes[s.ID])
			again = append(again, s)
		}
	}
	rep, err := translate.Translate(ctx, []llm.Client{m}, again, translate.Options{VocabFiles: files, MaxRepairs: opt.MaxRepairs, BatchSize: opt.Batch, Parallel: opt.Parallel})
	if err != nil {
		return fmt.Errorf("cover: translate: %w", err)
	}
	st.Usage.Add(rep.Usage)
	st.Errors = append(st.Errors, rep.BatchErrors...)
	fresh := map[string][]translate.Candidate{}
	for _, c := range rep.Candidates {
		fresh[c.StatementID] = append(fresh[c.StatementID], c)
	}
	var merged []translate.Candidate
	for _, id := range ids {
		cs := fresh[id]
		valid := len(cs) > 0
		for _, c := range cs {
			if c.Outcome == "rule" && c.Error != "" {
				valid = false
			}
		}
		if !valid {
			cs = old[id]
		} else if keep, ok := restore(id, cs); ok {
			cs = keep
		}
		merged = append(merged, cs...)
	}
	st.Cands = append(kept, merged...)
	sort.SliceStable(st.Cands, func(i, j int) bool { return order[st.Cands[i].StatementID] < order[st.Cands[j].StatementID] })
	return nil
}

// CoverageSummary renders the clause coverage for the owner: the counts,
// then every statement with an uncovered clause.
func CoverageSummary(res *Result) string {
	cs := res.Cover
	if cs.Statements == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Clause coverage: %d statements with rules, %d clauses: %d covered, %d uncovered, %d not checkable; %d statements with a rule broader than their text.", cs.Statements, cs.Clauses, cs.Covered, cs.Uncovered, cs.NotCheckable, cs.Broader)
	if cs.Retranslated > 0 {
		fmt.Fprintf(&b, " %d statements translated again with a finding named; %d then faithful.", cs.Retranslated, cs.Recovered)
	}
	if cs.Errors > 0 {
		fmt.Fprintf(&b, " %d statements could not be checked.", cs.Errors)
	}
	return b.String()
}

// restoreRereads puts back the earlier rules of statements whose reread a
// stopped run left unfinished, so a resumed run never loses them; the step
// that asked for the reread runs again and asks again.
func restoreRereads(st *state, stmts []translate.Statement) bool {
	if len(st.Rereading) == 0 {
		return false
	}
	has := map[string]bool{}
	for _, c := range st.Cands {
		has[c.StatementID] = true
	}
	for id, old := range st.Rereading {
		if !has[id] {
			st.Cands = append(st.Cands, old...)
		}
		delete(st.CompactErrors, id)
	}
	st.Rereading = nil
	order := map[string]int{}
	for i, s := range stmts {
		order[s.ID] = i
	}
	sort.SliceStable(st.Cands, func(i, j int) bool { return order[st.Cands[i].StatementID] < order[st.Cands[j].StatementID] })
	return true
}

// hasRule reports whether some candidate is a rule that compiles.
func hasRule(cs []translate.Candidate) bool {
	return slices.ContainsFunc(cs, func(c translate.Candidate) bool { return c.Outcome == "rule" && c.Error == "" })
}

func candidatesOf(cs []translate.Candidate, id string) []translate.Candidate {
	var out []translate.Candidate
	for _, c := range cs {
		if c.StatementID == id {
			out = append(out, c)
		}
	}
	return out
}

// worse reports whether coverage a is worse than b: fewer clauses covered,
// or as many covered with more uncovered clauses and rules beyond the text.
func worse(a, b StatementCoverage) bool {
	covered := func(sc StatementCoverage) (n int) {
		for _, c := range sc.Clauses {
			if c.Status == Covered {
				n++
			}
		}
		return n
	}
	if ca, cb := covered(a), covered(b); ca != cb {
		return ca < cb
	}
	return len(a.uncovered())+len(a.Broader) > len(b.uncovered())+len(b.Broader)
}

// sortByStatement orders candidates as their statements are.
func sortByStatement(cs []translate.Candidate, stmts []translate.Statement) {
	order := map[string]int{}
	for i, s := range stmts {
		order[s.ID] = i
	}
	sort.SliceStable(cs, func(i, j int) bool { return order[cs[i].StatementID] < order[cs[j].StatementID] })
}
