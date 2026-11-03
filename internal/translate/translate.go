// Package translate turns spec prose into candidate IR rules with an LLM
// (docs/07-translation-and-grounding.md). It never writes to a registry: it
// produces a candidate registry directory that `stele check` and `stele score`
// read like any other.
//
// v0 implements segment-free translation of given statements, validation
// against the symbol table, and repair of rules that fail to compile. Sampling,
// clustering and rendering for approval (docs/07 steps 4-7) are not built yet.
package translate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
)

// Statement is one requirement to translate.
type Statement struct {
	ID        string   `yaml:"id"`
	Source    string   `yaml:"source"`
	Prose     string   `yaml:"prose"`
	Reference []string `yaml:"reference"` // hand-written rules, for scoring only; never shown to the model
	// Introduced is the date the statement's current text was written (from
	// git), used to order statements in adoption triage; empty when unknown.
	Introduced string `yaml:"introduced,omitempty"`
	// Notes are hints for the translator from the vocabulary step, such as
	// "the vocabulary says Order.paymentState means 'payment taken'".
	Notes []string `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// LoadStatements reads a YAML list of statements.
func LoadStatements(path string) ([]Statement, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Statement
	if err := yaml.Unmarshal(src, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, s := range out {
		if s.ID == "" || s.Prose == "" || seen[s.ID] {
			return nil, fmt.Errorf("%s: every statement needs a unique id and prose (%q)", path, s.ID)
		}
		seen[s.ID] = true
	}
	return out, nil
}

// Candidate is one translated rule as the model returns it.
type Candidate struct {
	StatementID  string   `json:"statement_id"`
	Rationale    string   `json:"rationale"`
	Outcome      string   `json:"outcome"`
	Kind         string   `json:"kind"`
	Subject      string   `json:"subject"`
	On           string   `json:"on"`
	Operation    string   `json:"operation"`
	Effect       string   `json:"effect"`
	When         string   `json:"when"`
	Assert       string   `json:"assert"`
	MissingTerms []string `json:"missing_terms"`
	// Overrides names statements this rule takes precedence over because its
	// text says so (compact replies).
	Overrides []string `json:"overrides,omitempty"`
	// Assume makes a world fact on a step an assumption about the
	// situation it runs in (ir.Rule.Assume).
	Assume bool `json:"assume,omitempty"`

	RuleID string `json:"rule_id"`         // assigned: statement ID, then .2, .3 for further rules
	Error  string `json:"error,omitempty"` // validation error left after repair
	Draft  bool   `json:"draft,omitempty"` // the samples disagreed; written as draft until a person answers
}

// Options control a translation run.
type Options struct {
	VocabFiles []string
	MaxRepairs int // rounds of feeding validation errors back; default 2
	BatchSize  int // statements per request; 0 sends all in one
	Parallel   int // concurrent requests; default 1
	// Samples is the number of independent translations per model (default
	// 1). With more than one translation in total, each statement's readings
	// are clustered by solver equivalence (docs/07 steps 4-5), which needs Solver.
	Samples int
	Solver  *solver.Z3
}

// Report is everything a run produced, for the record.
type Report struct {
	Backend     string      `json:"backend"`
	Samples     []string    `json:"samples,omitempty"` // one label per translation: model#n
	Model       string      `json:"model,omitempty"`
	Statements  int         `json:"statements"`
	Candidates  []Candidate `json:"candidates"`
	Rounds      int         `json:"rounds"` // requests sent, over all batches
	Usage       llm.Usage   `json:"usage"`
	Invalid     int         `json:"invalid"`
	BatchErrors []string    `json:"batch_errors,omitempty"`
	// Clusters holds, per statement, the distinct readings the samples gave,
	// and a question for the author where they disagree.
	Clusters []StatementClusters `json:"clusters,omitempty"`
	// Inputs are the translated statements, kept so answers can be applied later.
	Inputs []Statement `json:"inputs,omitempty"`
}

// Translate translates stmts against the vocabulary with every model, Samples
// times each, and validates every rule, feeding compile errors back for
// repair. With one translation in total the result is that translation. With
// more, each statement's readings are clustered by solver equivalence: the
// largest cluster is kept, as an active rule when all readings agree and as
// a draft with a question for the author when they do not. A failed request
// is recorded in BatchErrors and its statements get no candidates from it.
func Translate(ctx context.Context, models []llm.Client, stmts []Statement, opt Options) (*Report, error) {
	vocab, err := ir.LoadFiles(opt.VocabFiles...)
	if err != nil {
		return nil, fmt.Errorf("vocabulary: %w", err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no model to translate with")
	}
	if opt.MaxRepairs == 0 {
		opt.MaxRepairs = 2
	}
	if opt.BatchSize <= 0 || opt.BatchSize > len(stmts) {
		opt.BatchSize = len(stmts)
	}
	if opt.Parallel <= 0 {
		opt.Parallel = 1
	}
	if opt.Samples <= 0 {
		opt.Samples = 1
	}
	total := len(models) * opt.Samples
	if total > 1 && opt.Solver == nil {
		return nil, fmt.Errorf("clustering %d samples needs a solver", total)
	}
	system := systemPrompt + RenderVocabulary(vocab)

	var batches [][]Statement
	for i := 0; i < len(stmts); i += opt.BatchSize {
		batches = append(batches, stmts[i:min(i+opt.BatchSize, len(stmts))])
	}
	type task struct{ sample, batch int }
	var tasks []task
	var labels []string
	for mi, m := range models {
		for k := 0; k < opt.Samples; k++ {
			labels = append(labels, fmt.Sprintf("%s#%d", m.Name(), k+1))
			for b := range batches {
				tasks = append(tasks, task{sample: mi*opt.Samples + k, batch: b})
			}
		}
	}
	results := make([][]batchResult, total)
	for i := range results {
		results[i] = make([]batchResult, len(batches))
	}
	sem := make(chan struct{}, opt.Parallel)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m := models[t.sample/opt.Samples]
			results[t.sample][t.batch] = translateBatch(ctx, m, system, vocab, batches[t.batch], opt)
		}()
	}
	wg.Wait()

	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name()
	}
	rep := &Report{Statements: len(stmts), Backend: strings.Join(names, ","), Samples: labels, Inputs: stmts}
	samples := make([][]Candidate, total)
	failed := 0
	for k, rs := range results {
		for b, r := range rs {
			rep.Rounds += r.rounds
			rep.Usage.Add(r.usage)
			if r.model != "" {
				rep.Model = r.model
			}
			if r.err != nil {
				failed++
				rep.BatchErrors = append(rep.BatchErrors, fmt.Sprintf("%s batch %d (%s..%s): %v", labels[k], b+1, batches[b][0].ID, batches[b][len(batches[b])-1].ID, r.err))
			}
			samples[k] = append(samples[k], r.cands...)
		}
	}
	if failed == len(tasks) {
		return rep, fmt.Errorf("every request failed: %s", rep.BatchErrors[0])
	}
	if total == 1 {
		rep.Candidates = samples[0]
	} else {
		if err := cluster(ctx, rep, stmts, samples, labels, opt); err != nil {
			return rep, err
		}
	}
	for _, c := range rep.Candidates {
		if c.Error != "" {
			rep.Invalid++
		}
	}
	return rep, nil
}

type batchResult struct {
	cands  []Candidate
	rounds int
	usage  llm.Usage
	model  string
	err    error
}

func translateBatch(ctx context.Context, m llm.Client, system string, vocab *ir.Registry, stmts []Statement, opt Options) batchResult {
	var req strings.Builder
	req.WriteString("Translate each statement below into Stele IR rules. Under each statement are existing symbols whose names share its words; use them when they mean what the statement means.\n\n")
	for _, s := range stmts {
		fmt.Fprintf(&req, "statement_id: %s\nsource: %s\ntext: %s\n", s.ID, s.Source, s.Prose)
		if rel := Related(vocab, s.Prose, 12); len(rel) > 0 {
			fmt.Fprintf(&req, "related symbols: %s\n", strings.Join(rel, ", "))
		}
		for _, n := range s.Notes {
			fmt.Fprintf(&req, "vocabulary note: %s\n", n)
		}
		req.WriteString("\n")
	}
	turns := []llm.Turn{{Role: "user", Text: req.String()}}
	var res batchResult
	for round := 0; ; round++ {
		reply, err := m.Complete(ctx, llm.Request{System: system, Turns: turns, Schema: outputSchema})
		if err != nil {
			res.err = err
			return res
		}
		res.rounds++
		res.model = reply.Model
		res.usage.Add(reply.Usage)
		cands, err := parseReply(reply.JSON)
		if err != nil {
			res.err = err
			return res
		}
		problems := complete(cands, stmts)
		assignIDs(cands)
		for i := range cands {
			cands[i].Error = validate(opt.VocabFiles, &cands[i])
			if cands[i].Error != "" {
				problems = append(problems, fmt.Sprintf("%s (statement %s): %s", cands[i].RuleID, cands[i].StatementID, cands[i].Error))
			}
		}
		res.cands = cands
		if len(problems) == 0 || round >= opt.MaxRepairs {
			return res
		}
		turns = append(turns, llm.Turn{Role: "assistant", Text: reply.JSON}, llm.Turn{Role: "user", Text: "Some entries do not compile against the vocabulary:\n- " + strings.Join(problems, "\n- ") +
			"\n\nReturn the complete corrected list for all statements. Fix only what the errors point at; if a statement cannot be expressed with the vocabulary, use needs_vocabulary."})
	}
}

func parseReply(text string) ([]Candidate, error) {
	var out struct {
		Rules []Candidate `json:"rules"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("reply is not the expected JSON: %w\n%.2000s", err, text)
	}
	if out.Rules == nil {
		return nil, fmt.Errorf("reply has no rules: %.2000s", text)
	}
	return out.Rules, nil
}

// complete reports statements with no entry and entries for unknown statements.
func complete(cands []Candidate, stmts []Statement) []string {
	var problems []string
	known := map[string]bool{}
	got := map[string]bool{}
	for _, s := range stmts {
		known[s.ID] = true
	}
	for _, c := range cands {
		got[c.StatementID] = true
		if !known[c.StatementID] {
			problems = append(problems, fmt.Sprintf("unknown statement_id %q", c.StatementID))
		}
	}
	for _, s := range stmts {
		if !got[s.ID] {
			problems = append(problems, fmt.Sprintf("statement %s has no entry", s.ID))
		}
	}
	return problems
}

func assignIDs(cands []Candidate) {
	n := map[string]int{}
	for i := range cands {
		id := cands[i].StatementID
		n[id]++
		cands[i].RuleID = id
		if n[id] > 1 {
			cands[i].RuleID = fmt.Sprintf("%s.%d", id, n[id])
		}
	}
}

// StatementOf maps a candidate rule ID back to its statement ID.
func StatementOf(ruleID string) string {
	if i := strings.LastIndexByte(ruleID, '.'); i > 0 && strings.Trim(ruleID[i+1:], "0123456789") == "" {
		return ruleID[:i]
	}
	return ruleID
}

// TextHash identifies a statement's text, so sync can tell an edited
// statement from a moved one (docs/08 W5 step 9).
func TextHash(prose string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(prose)))
	return hex.EncodeToString(sum[:8])
}

// ruleDoc is the YAML document for a candidate.
func ruleDoc(c *Candidate, prose map[string]Statement) map[string]any {
	status := "active"
	if c.Draft {
		status = "draft"
	}
	d := map[string]any{"id": c.RuleID, "status": status}
	switch {
	case c.Outcome == ir.Obligation:
		d["kind"] = ir.Obligation
		d["rationale"] = c.Rationale
	case c.Outcome != "rule":
		d["kind"] = ir.Informal
		d["rationale"] = c.Rationale // which clause is left to review, and why
	default:
		d["kind"] = c.Kind
		for k, v := range map[string]string{"subject": c.Subject, "on": c.On, "operation": c.Operation, "when": c.When, "assert": c.Assert} {
			if v != "" {
				d[k] = v
			}
		}
		if c.Kind == ir.Permission {
			d["effect"] = c.Effect
		}
		if len(c.Overrides) > 0 {
			d["overrides"] = c.Overrides
		}
		if c.Assume {
			d["assume"] = true
		}
	}
	src := map[string]any{"translatedBy": "stele translate", "statementId": c.StatementID}
	if s, ok := prose[c.StatementID]; ok {
		src["doc"], src["prose"], src["textHash"] = s.Source, s.Prose, TextHash(s.Prose)
		src["statementHash"] = TextHash(s.Prose)
		if s.Introduced != "" {
			src["introduced"] = s.Introduced
		}
	}
	if len(c.MissingTerms) > 0 {
		src["missingTerms"] = c.MissingTerms
	}
	d["source"] = src
	return d
}

func marshalDocs(docs []map[string]any) ([]byte, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), enc.Close()
}

// Revalidate compiles every candidate rule again against the vocabulary
// files and records the errors, for a vocabulary that changed after the
// translation.
func Revalidate(vocabFiles []string, cands []Candidate) {
	for i := range cands {
		cands[i].Error = validate(vocabFiles, &cands[i])
	}
}

// validate compiles one candidate against the vocabulary and returns the
// error text, or "".
func validate(vocabFiles []string, c *Candidate) string {
	if c.Outcome != "rule" {
		return ""
	}
	src, err := marshalDocs([]map[string]any{ruleDoc(c, nil)})
	if err != nil {
		return err.Error()
	}
	dir, err := os.MkdirTemp("", "stele-validate-")
	if err != nil {
		return err.Error()
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "candidate.yaml")
	if err := os.WriteFile(f, src, 0o644); err != nil {
		return err.Error()
	}
	reg, err := ir.LoadFiles(append(append([]string{}, vocabFiles...), f)...)
	if err != nil {
		return strings.ReplaceAll(err.Error(), f+":", "")
	}
	if _, err := lower.Lower(reg, lower.Options{}); err != nil {
		return err.Error()
	}
	return ""
}

// WriteRegistry writes the vocabulary files and the translated rules into dir,
// plus report.json.
func WriteRegistry(dir string, rep *Report, stmts []Statement, vocabFiles []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Files already in dir stay as they are; one copied in from elsewhere
	// never takes the name of another (an input vocabulary.yaml beside the
	// run's own proposal), and is named input-<name> instead.
	taken := map[string]bool{"rules.yaml": true, "report.json": true}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	inside := func(f string) bool {
		a, err := filepath.Abs(f)
		return err == nil && filepath.Dir(a) == abs
	}
	for _, f := range vocabFiles {
		if inside(f) {
			taken[filepath.Base(f)] = true
		}
	}
	for _, f := range vocabFiles {
		if inside(f) {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		name := filepath.Base(f)
		for taken[name] {
			name = "input-" + name
		}
		taken[name] = true
		if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
			return err
		}
	}
	prose := map[string]Statement{}
	for _, s := range stmts {
		prose[s.ID] = s
	}
	var docs []map[string]any
	for i := range rep.Candidates {
		c := &rep.Candidates[i]
		if c.Error != "" {
			continue // a rule that does not compile is reported, not written
		}
		docs = append(docs, ruleDoc(c, prose))
	}
	src, err := marshalDocs(docs)
	if err != nil {
		return err
	}
	header := []byte("# Translated by `stele translate`; see report.json. Not reviewed.\n")
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), append(header, src...), 0o644); err != nil {
		return err
	}
	js, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(js, '\n'), 0o644); err != nil {
		return err
	}
	q := filepath.Join(dir, "questions.md")
	if md := questionsMarkdown(rep, stmts); md != "" {
		return os.WriteFile(q, []byte(md), 0o644)
	}
	if err := os.Remove(q); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
