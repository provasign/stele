// Package formalize turns a whole spec corpus into a registry: vocabulary
// proposed from the statements, every statement translated, and the gaps the
// translator reports closed by extending the vocabulary and translating again
// (docs/07). Nothing in it depends on the project: the only input is the
// statements, plus whatever vocabulary already exists.
//
// The steps:
//
//  1. Propose vocabulary for all statements (facts first, then operations).
//  2. Translate every statement.
//  3. Close gaps, up to Rounds times: for statements with parts the
//     translator could not express, give their missing terms to the
//     vocabulary step, which maps each to an existing symbol or proposes
//     one, then translate those statements again with the mappings as notes.
//  4. Infer writes: a rule about what holds after an operation constrains
//     attributes that operation changes, so the operation writes them,
//     unless the world sets them.
//  5. Prune symbols no rule uses, so unused operations do not become dead
//     responses and the owner reviews only what is used.
//  6. Give every statement an end state, and measure how often statements
//     from different sources meet on a shared attribute.
//
// Each stage saves its state, so a run that stops can resume.
package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lesson"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// Files written into the output directory.
const (
	VocabularyFile = "vocabulary.yaml"
	LinksFile      = "links.yaml"
	StateFile      = "formalize-state.json"
	ReportFile     = "formalize.json"
	CoverageFile   = "coverage.md"
)

// End states of a statement.
const (
	Translated      = "translated"       // every part is a rule
	Partial         = "partial"          // rules, plus parts with no checkable content
	Informal        = "informal"         // no checkable content
	NeedsVocabulary = "needs_vocabulary" // parts still lack vocabulary after every round: a question for the owner
	Obligation      = "obligation"       // about the code or the process: checked against the repository (Phase 2)
	Invalid         = "invalid"          // no rule that compiles, or no reply
)

// Options control a run.
type Options struct {
	VocabFiles []string // existing vocabulary; at least a context file
	Context    string   // context new symbols belong to
	Rounds     int      // gap-closing rounds; default 3
	VocabBatch int      // statements per vocabulary request; default 40
	FactBatch  int      // statements per facts request; default 150
	Batch      int      // statements per translation request; default 40
	Parallel   int      // concurrent translation requests; default 4
	MaxRepairs int
	NoPrune    bool
	// NoConsolidate skips merging used attributes that name one fact twice.
	NoConsolidate bool
	// FactMap also asks once over every used attribute which name one fact
	// and which facts tie others by their nature (ISSUES 16, 17).
	FactMap bool
	// OnePass builds vocabulary and rules in one request per chunk of
	// OnePassChunk statements, instead of vocabulary first.
	OnePass      bool
	OnePassChunk int
	// Compact uses a smaller wire format and checkpoints individual statements.
	// Batch limits statements; BatchBytes limits source text per request.
	Compact    bool
	BatchBytes int
	// FactsFirst, with Compact, proposes the vocabulary's facts over the
	// whole corpus before the compact requests, which then share its names.
	FactsFirst bool
	// AcrossDocuments lets one compact request hold statements from several
	// documents, so a large enough batch sends the corpus in one request.
	AcrossDocuments bool
	// Retrieve, when positive, sends each compact request only the existing
	// vocabulary related to its statements (this many attributes, with their
	// entities and operations), plus everything this run proposed. Rules are
	// still compiled against the whole vocabulary.
	Retrieve int
	MaxCalls int // total journaled calls allowed in this output directory; 0 is unlimited
	// MaxCost is the most this run's model calls may cost, as reported: a
	// call starts only when its expected cost (from the cost per prompt byte
	// of the calls so far) fits with the calls in flight, so only the first
	// call can pass it. Backends that report no cost are not limited by it.
	MaxCost        float64
	RequestTimeout time.Duration
	// Reconsolidate merges attributes that name one fact twice once more,
	// over the whole proposal, before the first translation.
	Reconsolidate bool
	// CodeContext is the structure of the project's code (types, steps),
	// shown to compact requests so the vocabulary follows it (D-053).
	CodeContext string
	// Known are attributes of the existing vocabulary, each with the text of
	// the statements whose rules read it. Consolidation compares them with
	// the attributes a run proposes, so a new name for an existing fact is
	// merged into it or flagged, even when no rule of this run reads it.
	Known []vocab.Used
	// Others are statements not translated in this run that a rule may
	// still take precedence over by naming them (FR-003b).
	Others []translate.Statement
	// NoCover skips the clause coverage check (cover.go). CoverRounds below
	// zero checks but does not translate uncovered statements again.
	NoCover     bool
	CoverRounds int
	// Lessons are what the owner taught Stele about this project (D-063):
	// each statement gets the relevant ones as notes, and the coverage
	// judge the reading ones.
	Lessons []*ir.Lesson
	// NoSelfCheck skips checking each statement's rules against each other
	// (selfcheck.go); Z3 is the solver binary it uses (default z3).
	NoSelfCheck bool
	Z3          string
	Log         io.Writer
}

// Round records one gap-closing round.
type Round struct {
	Gaps       int `json:"gaps"`        // statements with parts needing vocabulary, before the round
	Missing    int `json:"missing"`     // missing terms named
	NewSymbols int `json:"new_symbols"` // attributes, relations and operations added
	Mappings   int `json:"mappings"`    // terms mapped to an existing symbol
	After      int `json:"after"`       // statements still with such parts after re-translating
}

// End is one statement's end state.
type End struct {
	ID      string   `json:"id"`
	Source  string   `json:"source"`
	State   string   `json:"state"`
	Rules   []string `json:"rules,omitempty"`
	Missing []string `json:"missing,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	// Uncovered are clauses of the statement that no rule checks, and
	// Broader the rules that forbid what its text allows (cover.go).
	Uncovered []string `json:"uncovered,omitempty"`
	Broader   []string `json:"broader,omitempty"`
	// Blocks says how a rule of the statement alone leaves a step no
	// valid outcome (selfcheck.go).
	Blocks string `json:"blocks,omitempty"`
	// Lessons are the owner's lessons the statement was read with.
	Lessons []string `json:"lessons,omitempty"`
}

// Health measures whether the vocabulary lets statements meet: two
// statements can only conflict on an attribute both of their rules read.
type Health struct {
	Statements      int `json:"statements"`
	WithRules       int `json:"with_rules"`
	Attributes      int `json:"attributes"`        // in the vocabulary, after pruning
	SharedAttrs     int `json:"shared_attributes"` // read by rules of two or more statements
	MeetingOtherSrc int `json:"meeting_other_source"`
	Operations      int `json:"operations"`
}

// Result is a run's record.
type Result struct {
	Backend  string            `json:"backend"`
	Ends     []End             `json:"statements"`
	Counts   map[string]int    `json:"counts"`
	Rounds   []Round           `json:"rounds"`
	Inferred []string          `json:"inferred_writes,omitempty"`
	Defaults []string          `json:"defaults_corrected,omitempty"`
	Pruned   []string          `json:"pruned,omitempty"`
	Health   Health            `json:"health"`
	Usage    llm.Usage         `json:"usage"`
	Calls    CallStats         `json:"calls"`
	Deferred map[string]string `json:"deferred,omitempty"` // excluded statements requiring review
	Flags    []vocab.Flag      `json:"possible_duplicates,omitempty"`
	Merges   []vocab.Merge     `json:"merges,omitempty"`
	Errors   []string          `json:"errors,omitempty"`
	Mappings []vocab.Mapping   `json:"mappings,omitempty"`
	Links    []string          `json:"links,omitempty"`
	// Consolidated is false in the report written before consolidation, which
	// a run stopped during consolidation leaves behind.
	Consolidated bool                  `json:"consolidated"`
	Cover        CoverStats            `json:"clause_coverage"`
	SelfCheck    SelfStats             `json:"self_check"`
	Coverage     []StatementCoverage   `json:"clauses,omitempty"`
	Proposal     vocab.Proposal        `json:"-"`
	Cands        []translate.Candidate `json:"-"`
	LinkCands    []translate.Candidate `json:"-"` // the world facts in Links
}

// state is what a run saves between stages.
type state struct {
	Stage     string `json:"stage"` // one-pass, vocabulary, translated, round-N, done
	InputHash string `json:"input_hash,omitempty"`
	// OnePassNext is the first statement not yet accepted, independent of chunk size.
	OnePassNext     int                   `json:"one_pass_next,omitempty"`
	CompactErrors   map[string]string     `json:"compact_errors,omitempty"`
	CompactDeferred map[string]string     `json:"compact_deferred,omitempty"`
	Proposal        vocab.Proposal        `json:"proposal"`
	Cands           []translate.Candidate `json:"candidates"`
	Notes           map[string][]string   `json:"notes,omitempty"`
	Rounds          []Round               `json:"rounds,omitempty"`
	Usage           llm.Usage             `json:"usage"`
	Flags           []vocab.Flag          `json:"possible_duplicates,omitempty"`
	ValueMerges     []ValueMerge          `json:"value_merges,omitempty"`
	Merges          []vocab.Merge         `json:"merges,omitempty"`
	Errors          []string              `json:"errors,omitempty"`
	Mappings        []vocab.Mapping       `json:"mappings,omitempty"`
	// Closed is set when gap closing has finished, so a rerun goes
	// straight to the final steps.
	Closed bool `json:"gaps_closed,omitempty"`
	// Consolidated is set once the used attributes were consolidated and
	// the statements that read a merged one translated again.
	Consolidated bool `json:"consolidated,omitempty"`
	// Links are world facts relating two attributes, found by consolidation.
	Links []translate.Candidate `json:"links,omitempty"`
	// Covered is set once the clause coverage check ran (cover.go).
	Covered    bool                `json:"covered,omitempty"`
	Coverage   []StatementCoverage `json:"clause_coverage,omitempty"`
	Uncovered  map[string][]string `json:"uncovered,omitempty"`
	Exceeds    map[string][]string `json:"exceeds,omitempty"` // rules beyond their text, described
	CoverStats CoverStats          `json:"cover_stats,omitempty"`
	// SelfChecked is set once the self-conflict check ran (selfcheck.go);
	// SelfConflicts are the statements whose rules still contradict each
	// other, with the clash, and whose rules are held out.
	// Rereading holds the earlier rules of statements being read again,
	// until the new reading is accepted (restoreRereads).
	Rereading     map[string][]translate.Candidate `json:"rereading,omitempty"`
	SelfChecked   bool                             `json:"self_checked,omitempty"`
	SelfConflicts map[string]string                `json:"self_conflicts,omitempty"`
	Blocking      map[string]string                `json:"blocking,omitempty"` // statements with a rule that alone blocks a step
	SelfStats     SelfStats                        `json:"self_stats,omitempty"`
}

// Run formalizes stmts into dir. It resumes from dir's saved state when
// there is one.
func Run(ctx context.Context, m llm.Client, stmts []translate.Statement, dir string, opt Options) (*Result, error) {
	if opt.Compact && opt.OnePass {
		return nil, fmt.Errorf("choose compact or one-pass mode, not both")
	}
	if len(stmts) == 0 {
		return nil, fmt.Errorf("no statements to formalize")
	}
	if opt.Rounds == 0 {
		opt.Rounds = 3
	}
	if opt.Context == "" {
		opt.Context = "main"
	}
	if opt.Log == nil {
		opt.Log = io.Discard
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	st, err := loadState(dir)
	if err != nil {
		return nil, err
	}
	// What the owner taught Stele about this project reaches every request
	// as notes (D-063). The notes are part of the input, so a run resumes
	// only with the lessons it started with.
	if len(opt.Lessons) > 0 {
		stmts = slices.Clone(stmts)
	}
	applied := applyLessons(stmts, opt.Lessons)
	if err := bindInputs(dir, st, stmts, opt); err != nil {
		return nil, err
	}
	tracked, err := newCallJournal(m, dir, opt)
	if err != nil {
		return nil, err
	}
	defer tracked.close()
	m = tracked
	vopt := vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context, BatchSize: opt.VocabBatch, MaxRepairs: opt.MaxRepairs, Facts: true, FactBatch: opt.FactBatch, Reconsolidate: opt.Reconsolidate, Log: opt.Log}
	save := func(stage string) error {
		// Some legacy phases collect model errors and continue. A hard budget
		// or cancellation must not turn that incomplete phase into a checkpoint.
		if err := tracked.stopErr(); err != nil {
			return err
		}
		st.Stage = stage
		return saveState(dir, st)
	}
	if opt.Compact && opt.FactsFirst && st.Stage == "" {
		// One fact, one name: propose the facts over the whole corpus first,
		// so every compact request reads and reuses the same names (ISSUES
		// 16). Compact requests still propose what the seed lacks.
		fmt.Fprintf(opt.Log, "facts first: proposing the vocabulary over all %d statement(s) ...\n", len(stmts))
		rep, err := vocab.Propose(ctx, m, stmts, vopt)
		if err != nil {
			return nil, fmt.Errorf("facts first: %w", err)
		}
		st.Proposal, st.Flags, st.Merges = rep.Proposal, rep.Flags, rep.Merges
		st.Usage.Add(rep.Usage)
		st.Errors = append(st.Errors, rep.Errors...)
		fmt.Fprintf(opt.Log, "facts first: %s, %d fact(s) merged, about $%.2f\n", describe(st.Proposal), len(rep.Merges), rep.Usage.CostUSD)
		if err := save("facts"); err != nil {
			return nil, err
		}
	}
	if opt.Compact && (st.Stage == "" || st.Stage == "facts" || st.Stage == "compact") {
		if err := save("compact"); err != nil {
			return nil, err
		}
		if err := compactPass(ctx, m, stmts, st, opt, dir); err != nil {
			return nil, stopped(err, dir, m, stmts, st, opt, tracked)
		}
		// Compact replies already propose missing vocabulary. Remaining gaps
		// denote unsupported concepts, not a request for a speculative ontology.
		st.Closed = true
		if err := save("translated"); err != nil {
			return nil, err
		}
	}

	if opt.OnePass && (st.Stage == "" || st.Stage == "one-pass") {
		if err := save("one-pass"); err != nil {
			return nil, err
		}
		fmt.Fprintf(opt.Log, "one pass: vocabulary and rules for %d statement(s) ...\n", len(stmts))
		if err := onePass(ctx, m, stmts, st, opt, dir); err != nil {
			return nil, stopped(err, dir, m, stmts, st, opt, tracked)
		}
		fmt.Fprintf(opt.Log, "one pass: %s; %s, about $%.2f\n", describe(st.Proposal), outcomes(st.Cands), st.Usage.CostUSD)
		if err := save("translated"); err != nil {
			return nil, err
		}
	}
	if st.Stage == "" {
		fmt.Fprintf(opt.Log, "vocabulary: proposing for %d statement(s) ...\n", len(stmts))
		rep, err := vocab.Propose(ctx, m, stmts, vopt)
		if err != nil {
			return nil, fmt.Errorf("vocabulary: %w", err)
		}
		st.Proposal, st.Flags, st.Merges = rep.Proposal, rep.Flags, rep.Merges
		st.Usage.Add(rep.Usage)
		st.Errors = append(st.Errors, rep.Errors...)
		fmt.Fprintf(opt.Log, "vocabulary: %s, %d fact(s) merged, about $%.2f\n", describe(st.Proposal), len(rep.Merges), rep.Usage.CostUSD)
		if err := save("vocabulary"); err != nil {
			return nil, err
		}
	}
	files, err := writeVocabulary(dir, st.Proposal, opt)
	if err != nil {
		return nil, err
	}

	if st.Stage == "vocabulary" {
		fmt.Fprintf(opt.Log, "translate: %d statement(s) ...\n", len(stmts))
		rep, err := translate.Translate(ctx, []llm.Client{m}, stmts, translate.Options{VocabFiles: files, MaxRepairs: opt.MaxRepairs, BatchSize: opt.Batch, Parallel: opt.Parallel})
		if err != nil {
			return nil, fmt.Errorf("translate: %w", err)
		}
		st.Cands = rep.Candidates
		st.Usage.Add(rep.Usage)
		st.Errors = append(st.Errors, rep.BatchErrors...)
		fmt.Fprintf(opt.Log, "translate: %s, about $%.2f\n", outcomes(st.Cands), rep.Usage.CostUSD)
		if err := save("translated"); err != nil {
			return nil, err
		}
	}

	for !st.Closed && len(st.Rounds) < opt.Rounds {
		gaps := gapsOf(stmts, st.Cands)
		if len(gaps) == 0 {
			break
		}
		r := Round{Gaps: len(gaps)}
		for _, g := range gaps {
			r.Missing += len(g.Missing)
		}
		fmt.Fprintf(opt.Log, "round %d: %d statement(s) with %d missing term(s) ...\n", len(st.Rounds)+1, r.Gaps, r.Missing)
		vrep := &vocab.Report{}
		added, maps, err := vocab.Extend(ctx, m, gaps, st.Proposal, vopt, vrep)
		st.Usage.Add(vrep.Usage)
		st.Errors = append(st.Errors, vrep.Errors...)
		if err != nil {
			return nil, fmt.Errorf("round %d: %w", len(st.Rounds)+1, err)
		}
		r.NewSymbols, r.Mappings = count(added), len(maps)
		st.Mappings = append(st.Mappings, maps...)
		if r.NewSymbols == 0 && r.Mappings == 0 {
			r.After = r.Gaps
			st.Rounds = append(st.Rounds, r)
			fmt.Fprintf(opt.Log, "round %d: nothing to add; stopping\n", len(st.Rounds))
			if err := save(fmt.Sprintf("round-%d", len(st.Rounds))); err != nil {
				return nil, err
			}
			break
		}
		st.Proposal = vocab.Merged(st.Proposal, added)
		if files, err = writeVocabulary(dir, st.Proposal, opt); err != nil {
			return nil, err
		}
		if st.Notes == nil {
			st.Notes = map[string][]string{}
		}
		for _, mp := range maps {
			st.Notes[mp.StatementID] = append(st.Notes[mp.StatementID], fmt.Sprintf("for %q use %s (%s)", mp.Term, mp.Symbol, mp.Reason))
		}
		var again []translate.Statement
		for _, g := range gaps {
			s := g.Statement
			s.Notes = st.Notes[s.ID]
			again = append(again, s)
		}
		rep, err := translate.Translate(ctx, []llm.Client{m}, again, translate.Options{VocabFiles: files, MaxRepairs: opt.MaxRepairs, BatchSize: opt.Batch, Parallel: opt.Parallel})
		if err != nil {
			return nil, fmt.Errorf("round %d translate: %w", len(st.Rounds)+1, err)
		}
		st.Usage.Add(rep.Usage)
		st.Errors = append(st.Errors, rep.BatchErrors...)
		st.Cands = replace(st.Cands, rep.Candidates)
		r.After = len(gapsOf(stmts, st.Cands))
		st.Rounds = append(st.Rounds, r)
		fmt.Fprintf(opt.Log, "round %d: %d symbol(s) added, %d mapped; %d statement(s) still need vocabulary\n", len(st.Rounds), r.NewSymbols, r.Mappings, r.After)
		if err := save(fmt.Sprintf("round-%d", len(st.Rounds))); err != nil {
			return nil, err
		}
		if r.After >= r.Gaps {
			break // no progress
		}
	}

	if !st.Closed {
		st.Closed = true
		if err := save(st.Stage); err != nil {
			return nil, err
		}
	}

	if !st.Consolidated && !opt.NoConsolidate {
		// Write the translation before consolidating, so a run stopped by its
		// budget or deadline during consolidation still leaves a registry.
		if _, err := finish(dir, m, stmts, st, opt, tracked); err != nil {
			return nil, err
		}
		if err := consolidateUsed(ctx, m, stmts, st, opt, dir); err != nil {
			return nil, err
		}
		if opt.Compact {
			if err := consolidateValues(ctx, m, stmts, st, opt); err != nil {
				return nil, err
			}
		}
		st.Consolidated = true
		if err := save(st.Stage); err != nil {
			return nil, err
		}
	}
	if restoreRereads(st, stmts) {
		fmt.Fprintln(opt.Log, "resume: restored the earlier rules of statements whose reread the last run did not finish")
		if err := save(st.Stage); err != nil {
			return nil, err
		}
	}
	if !st.Covered && !opt.NoCover {
		if err := cover(ctx, m, stmts, st, opt, dir); err != nil {
			return nil, err
		}
		st.Covered = true
		if err := save(st.Stage); err != nil {
			return nil, err
		}
	}
	if !st.SelfChecked && !opt.NoSelfCheck {
		if err := selfCheck(ctx, m, stmts, st, opt, dir, tracked); err != nil {
			return nil, err
		}
		st.SelfChecked = true
		if err := save(st.Stage); err != nil {
			return nil, err
		}
	}
	res, err := finish(dir, m, stmts, st, opt, tracked)
	if err != nil {
		return nil, err
	}
	for i := range res.Ends {
		res.Ends[i].Lessons = applied[res.Ends[i].ID]
	}
	if len(applied) > 0 {
		if err := writeReport(dir, res); err != nil {
			return nil, err
		}
	}
	if err := save("done"); err != nil {
		return nil, err
	}
	return res, nil
}

// stopped writes the registry of the work accepted so far when the budget,
// the deadline or a cancellation stopped translation, so the statements
// already translated can be checked before the run resumes. It returns err,
// with any failure to write.
func stopped(err error, dir string, m llm.Client, stmts []translate.Statement, st *state, opt Options, tracked *callJournal) error {
	if tracked.stopErr() == nil || len(st.Cands) == 0 {
		return err
	}
	if _, werr := finish(dir, m, stmts, st, opt, tracked); werr != nil {
		return errors.Join(err, fmt.Errorf("write the accepted work: %w", werr))
	}
	return err
}

// finish writes the registry, coverage and report for the run so far. It
// does not change the saved state, so it can run before consolidation and
// again after.
func finish(dir string, m llm.Client, stmts []translate.Statement, st *state, opt Options, tracked *callJournal) (*Result, error) {
	resolveOverrides(append(append([]translate.Statement{}, stmts...), opt.Others...), st.Cands)
	files, err := writeVocabulary(dir, st.Proposal, opt)
	if err != nil {
		return nil, err
	}
	res := &Result{Backend: m.Name(), Rounds: st.Rounds, Usage: st.Usage, Calls: tracked.stats(), Deferred: st.CompactDeferred, Flags: st.Flags, Merges: st.Merges, Errors: st.Errors, Mappings: st.Mappings, Consolidated: st.Consolidated}
	p := st.Proposal
	reg, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, p)
	if err != nil {
		return nil, fmt.Errorf("final vocabulary: %w", err)
	}
	translate.Revalidate(files, st.Links)
	rules := parseRules(reg, append(append([]translate.Candidate{}, st.Cands...), st.Links...))
	p, res.Inferred = inferWrites(reg, p, rules)
	p, res.Defaults = restrictedDefaults(p, rules)
	p = unboundCounters(p, rules)
	if !opt.NoPrune {
		var pruned vocab.Proposal
		pruned, res.Pruned = prune(reg, p, rules, named(append(append([]translate.Candidate{}, st.Cands...), st.Links...)))
		if _, err := vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, pruned); err == nil {
			p = pruned
		} else {
			res.Pruned = nil
			res.Errors = append(res.Errors, "pruning skipped: "+err.Error())
		}
	}
	files, err = writeVocabulary(dir, p, opt)
	if err != nil {
		return nil, err
	}
	// The vocabulary grew after the first translation; check every rule
	// against the final one.
	translate.Revalidate(files, st.Cands)
	holdOut(st.Cands, st.SelfConflicts)
	translate.Revalidate(files, st.Links)
	final := &translate.Report{Backend: m.Name(), Statements: len(stmts), Candidates: st.Cands, Inputs: stmts}
	if err := translate.WriteRegistry(dir, final, stmts, files); err != nil {
		return nil, err
	}
	if res.Links, err = writeLinks(dir, st.Links); err != nil {
		return nil, err
	}
	if _, err := ir.LoadDir(dir); err != nil {
		return nil, fmt.Errorf("the registry written to %s does not load: %w", dir, err)
	}
	reg, err = vocab.Load(vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}, p)
	if err != nil {
		return nil, err
	}
	res.Proposal, res.Cands, res.LinkCands = p, st.Cands, st.Links
	res.Cover, res.Coverage, res.SelfCheck = st.CoverStats, st.Coverage, st.SelfStats
	res.Ends, res.Counts = ends(stmts, st.Cands, st.CompactDeferred, st.Uncovered, st.Exceeds, st.Blocking)
	res.Health = health(reg, stmts, parseRules(reg, st.Cands), res.Ends)
	if err := writeReport(dir, res); err != nil {
		return nil, err
	}
	return res, nil
}

func loadState(dir string) (*state, error) {
	src, err := os.ReadFile(filepath.Join(dir, StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return &state{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(src, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", StateFile, err)
	}
	return &st, nil
}

func saveState(dir string, st *state) error {
	js, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// Replace the checkpoint only once the full file is written. An interrupted
	// write must not truncate the previous completed stage or chunk.
	f, err := os.CreateTemp(dir, ".formalize-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(js, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, StateFile))
}

// writeVocabulary writes the proposal and returns the vocabulary files a
// translation uses: the existing ones, then the proposal.
func writeVocabulary(dir string, p vocab.Proposal, opt Options) ([]string, error) {
	src, err := vocab.YAML(p, opt.Context)
	if err != nil {
		return nil, err
	}
	header := "# Proposed by `stele formalize` from the statements. Review before use: accept, rename, merge or delete.\n"
	f := filepath.Join(dir, VocabularyFile)
	if err := os.WriteFile(f, append([]byte(header), src...), 0o644); err != nil {
		return nil, err
	}
	out := []string{}
	for _, v := range opt.VocabFiles {
		if filepath.Clean(v) != filepath.Clean(f) {
			out = append(out, v)
		}
	}
	return append(out, f), nil
}

// gapsOf lists the statements with parts the translator could not express
// for lack of vocabulary, with the terms it named.
func gapsOf(stmts []translate.Statement, cands []translate.Candidate) []vocab.Gap {
	by := map[string]*vocab.Gap{}
	for _, c := range cands {
		if c.Outcome != "needs_vocabulary" {
			continue
		}
		g := by[c.StatementID]
		if g == nil {
			g = &vocab.Gap{}
			by[c.StatementID] = g
		}
		g.Missing = append(g.Missing, c.MissingTerms...)
		g.Rationale = append(g.Rationale, c.Rationale)
	}
	var out []vocab.Gap
	for _, s := range stmts {
		if g := by[s.ID]; g != nil {
			g.Statement = s
			if len(g.Missing) == 0 {
				g.Missing = []string{"(not named; see the translator's reason)"}
			}
			out = append(out, *g)
		}
	}
	return out
}

// replace swaps in the new candidates of every statement they cover. A
// statement whose new translation came back empty keeps its old one.
func replace(old, fresh []translate.Candidate) []translate.Candidate {
	redone := map[string]bool{}
	for _, c := range fresh {
		redone[c.StatementID] = true
	}
	var out []translate.Candidate
	for _, c := range old {
		if !redone[c.StatementID] {
			out = append(out, c)
		}
	}
	return append(out, fresh...)
}

func count(p vocab.Proposal) int {
	n := len(p.Operations)
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		n += len(e.Attributes) + len(e.Relations)
	}
	return n
}

func describe(p vocab.Proposal) string {
	attrs := 0
	for _, e := range append(append([]vocab.Entity{}, p.Entities...), p.Extensions...) {
		attrs += len(e.Attributes)
	}
	return fmt.Sprintf("%d entit(ies), %d attribute(s), %d operation(s)", len(p.Entities), attrs, len(p.Operations))
}

func outcomes(cands []translate.Candidate) string {
	n := map[string]int{}
	invalid := 0
	for _, c := range cands {
		n[c.Outcome]++
		if c.Error != "" {
			invalid++
		}
	}
	return fmt.Sprintf("%d rule(s), %d informal, %d need vocabulary, %d invalid", n["rule"], n["informal"], n["needs_vocabulary"], invalid)
}

// parsedRule is a candidate rule with the attributes it reads.
type parsedRule struct {
	cand   *translate.Candidate
	op     string     // operation it is about
	after  []*ir.Attr // attributes its assert reads, for a rule on a step
	reads  []*ir.Attr // every attribute it reads
	walked []string   // entities and relations its paths walk through
}

// parseRules resolves the paths of every valid candidate rule.
func parseRules(reg *ir.Registry, cands []translate.Candidate) []parsedRule {
	var out []parsedRule
	for i := range cands {
		c := &cands[i]
		if c.Outcome != "rule" || c.Error != "" {
			continue
		}
		pr := parsedRule{cand: c, op: c.On}
		if pr.op == "" {
			pr.op = c.Operation
		}
		x := &ir.Rule{ID: c.RuleID, Subject: c.Subject, On: c.On, Operation: c.Operation}
		var exprs []ir.Expr
		for _, src := range []string{c.When, c.Assert} {
			var e ir.Expr
			if src != "" {
				e, _ = ir.ParseExpr(src)
			}
			exprs = append(exprs, e)
		}
		x.When, x.Assert = exprs[0], exprs[1]
		subject, _ := reg.SubjectOf(x)
		for i, e := range exprs {
			if e == nil {
				continue
			}
			after := i == 1 && c.On != ""
			for _, p := range ir.Paths(e) {
				if c.Kind == ir.Derivation && len(p.Segs) > 1 && p.Segs[len(p.Segs)-1] == "default" {
					p = &ir.Path{Segs: p.Segs[:len(p.Segs)-1]}
				}
				if a, err := reg.Resolve(p, subject); err == nil {
					pr.reads = append(pr.reads, a)
					// Only the step's own instance (self.x, or a root such
					// as config.x) can be what it writes: an instance
					// reached through a relation is another one, which the
					// checker never lets a step write.
					if after && len(p.Segs) == 2 {
						pr.after = append(pr.after, a)
					}
				}
				pr.walked = append(pr.walked, walk(reg, p, subject)...)
			}
			// Attributes read through a quantifier's variable are used too,
			// never written: the variable ranges over other instances. A
			// listing that reads every tool's lent date was taken to write
			// it, and then any step could have changed a tool (a developer
			// trial, 2026-11-03).
			for _, a := range reg.Reads(e, subject) {
				if !slices.Contains(pr.reads, a) {
					pr.reads = append(pr.reads, a)
					pr.walked = append(pr.walked, a.Entity.Name)
				}
			}
		}
		out = append(out, pr)
	}
	return out
}

// walk lists the entities and relations (as Entity.relation) a path passes.
func walk(reg *ir.Registry, p *ir.Path, subject *ir.Entity) []string {
	var cur *ir.Entity
	switch p.Segs[0] {
	case "self":
		cur = subject
	default:
		for n, e := range reg.Entities {
			if strings.ToLower(n[:1])+n[1:] == p.Segs[0] {
				cur = e
			}
		}
	}
	var out []string
	for _, s := range p.Segs[1:] {
		if cur == nil {
			break
		}
		out = append(out, cur.Name)
		r := cur.Relations[s]
		if r == nil {
			break
		}
		out = append(out, cur.Name+"."+s)
		cur = reg.Entities[r.To]
	}
	return out
}

// restrictedDefaults gives an operation of the proposal default allow when
// its rules only restrict it (deny rules and preconditions) and none allows
// it: "a shipped order MUST NOT be cancelled" says cancelling is otherwise
// possible, and under default deny the operation could never run, which
// the check reports as the specs' problem when it is the proposal's.
// Declared operations keep their default. It returns what it changed.
func restrictedDefaults(p vocab.Proposal, rules []parsedRule) (vocab.Proposal, []string) {
	out := p
	out.Operations = append([]vocab.Operation{}, p.Operations...)
	var changed []string
	for i, o := range out.Operations {
		if o.Default != "deny" {
			continue
		}
		var restricts []string
		allowed := false
		for _, r := range rules {
			c := r.cand
			if c.Kind != "permission" && c.Kind != "precondition" {
				continue
			}
			if !(&ir.Rule{Operation: c.Operation}).AppliesTo(o.Name) {
				continue
			}
			if c.Kind == "permission" && c.Effect != "deny" {
				allowed = true
				break
			}
			restricts = append(restricts, c.RuleID)
		}
		if allowed || len(restricts) == 0 {
			continue
		}
		o.Default = "allow"
		out.Operations[i] = o
		changed = append(changed, fmt.Sprintf("%s: default allow, since its rules only restrict it (%s)", o.Name, strings.Join(restricts, ", ")))
	}
	return out, changed
}

// inferWrites adds to an operation's writes each attribute a rule asserts
// about the state after it, unless the world sets it or it is a request or
// actor input. Only operations of the proposal change; it returns what it
// added.
func inferWrites(reg *ir.Registry, p vocab.Proposal, rules []parsedRule) (vocab.Proposal, []string) {
	ops := map[string]int{}
	for i, o := range p.Operations {
		ops[o.Name] = i
	}
	out := p
	out.Operations = append([]vocab.Operation{}, p.Operations...)
	var added []string
	seen := map[string]bool{}
	for _, r := range rules {
		if r.cand.On == "" {
			continue
		}
		op := reg.Operations[r.cand.On]
		i, ours := ops[r.cand.On]
		if op == nil || !ours {
			continue
		}
		for _, a := range r.after {
			q := a.Path()
			if a.Entity.Name == "Request" || a.Entity.Name == "Actor" || a.Entity.Name == "Result" {
				continue // inputs, and a step's result: no step writes them as state
			}
			if a.WorldSet && !op.WritesAttr(a) && !seen[op.Name+" "+q] {
				// A step may reset a value the world sets, to a fixed value
				// (ISSUES 19): "activity sets the minutes left to 60".
				if v := resetValue(r.cand, a, op); v != "" {
					seen[op.Name+" "+q] = true
					o := out.Operations[i]
					o.Writes = append(append([]string{}, o.Writes...), q)
					o.Effect = maps.Clone(o.Effect)
					if o.Effect == nil {
						o.Effect = map[string]string{}
					}
					o.Effect[a.Name] = v
					out.Operations[i] = o
					added = append(added, fmt.Sprintf("%s resets %s to %s (rule %s)", op.Name, q, v, r.cand.RuleID))
				}
				continue
			}
			if a.WorldSet || op.WritesAttr(a) || seen[op.Name+" "+q] {
				continue
			}
			seen[op.Name+" "+q] = true
			o := out.Operations[i]
			o.Writes = append(append([]string{}, o.Writes...), q)
			out.Operations[i] = o
			added = append(added, fmt.Sprintf("%s writes %s (rule %s asserts it after the step)", op.Name, q, r.cand.RuleID))
		}
	}
	sort.Strings(added)
	return out, added
}

// resetValue is the literal an unconditional rule on op sets a world-set
// attribute of op's own entity to (assert self.a = v), or "".
func resetValue(c *translate.Candidate, a *ir.Attr, op *ir.Operation) string {
	if c.When != "" || a.Entity != op.Entity {
		return ""
	}
	e, err := ir.ParseExpr(c.Assert)
	if err != nil {
		return ""
	}
	b, ok := e.(*ir.Binary)
	if !ok || b.Op != "=" {
		return ""
	}
	p, ok := b.L.(*ir.Path)
	if !ok || len(p.Segs) != 2 || p.Segs[0] != "self" || p.Segs[1] != a.Name {
		return ""
	}
	switch v := b.R.(type) {
	case *ir.IntLit, *ir.BoolLit, *ir.StrLit, *ir.DecLit, *ir.Ident:
		return v.String()
	}
	return ""
}

// prune drops the proposal's attributes, relations, operations and entities
// no rule uses. It returns the pruned symbols.
func prune(reg *ir.Registry, p vocab.Proposal, rules []parsedRule, names map[string]bool) (vocab.Proposal, []string) {
	usedAttr, usedOp, usedEnt := map[string]bool{}, map[string]bool{}, map[string]bool{}
	// What a rule that does not compile yet names is kept as used: pruned,
	// it could never compile. A loan history's entity, read only through
	// instances(LoanRecord), was pruned with its operation, and the clause
	// went unchecked without a finding (a developer trial, 2026-11-03).
	for _, e := range p.Entities {
		if names[e.Name] {
			usedEnt[e.Name] = true
			for _, a := range e.Attributes {
				if names["."+a.Name] {
					usedAttr[e.Name+"."+a.Name] = true
				}
			}
			for _, r := range e.Relations {
				if names["."+r.Name] {
					usedAttr[e.Name+"."+r.Name] = true
				}
			}
		}
	}
	// An extension's members are read through self or a variable as often
	// as through the entity's root ("self.lastLoan.memberEmail"), so a
	// member is kept when any failing rule names it.
	for _, e := range p.Extensions {
		for _, a := range e.Attributes {
			if names["."+a.Name] {
				usedAttr[e.Name+"."+a.Name] = true
			}
		}
		for _, r := range e.Relations {
			if names["."+r.Name] {
				usedAttr[e.Name+"."+r.Name] = true
			}
		}
	}
	for _, o := range p.Operations {
		if names[o.Name] {
			usedOp[o.Name] = true
		}
	}
	for _, r := range rules {
		for _, a := range r.reads {
			usedAttr[a.Path()] = true
			usedEnt[a.Owner()] = true
			if a.Owner() != a.Entity.Name { // a record field keeps its record
				usedAttr[strings.SplitN(a.Path(), ".", 3)[0]+"."+strings.SplitN(a.Path(), ".", 3)[1]] = true
			}
		}
		for _, w := range r.walked {
			if strings.Contains(w, ".") {
				usedAttr[w] = true // a relation, kept like an attribute
			} else {
				usedEnt[w] = true
			}
		}
		if r.op != "" {
			usedOp[r.op] = true
			if op := reg.Operations[r.op]; op != nil {
				usedEnt[op.Entity.Name] = true
			}
		}
	}
	var pruned []string
	var out vocab.Proposal
	for _, o := range p.Operations {
		if !usedOp[o.Name] {
			pruned = append(pruned, "operation "+o.Name)
			continue
		}
		var ws []string
		for _, w := range o.Writes {
			if usedAttr[w] {
				ws = append(ws, w)
			}
		}
		o.Writes = ws
		if len(o.Effect) > 0 {
			ent, _, _ := strings.Cut(o.Name, ".")
			o.Effect = maps.Clone(o.Effect)
			maps.DeleteFunc(o.Effect, func(a, _ string) bool { return !slices.Contains(ws, ent+"."+a) })
		}
		out.Operations = append(out.Operations, o)
	}
	for _, o := range out.Operations {
		ent, _, _ := strings.Cut(o.Name, ".")
		usedEnt[ent] = true
	}
	keep := func(e vocab.Entity, extension bool) (vocab.Entity, bool) {
		var as []vocab.Attribute
		for _, a := range e.Attributes {
			if usedAttr[e.Name+"."+a.Name] {
				as = append(as, a)
			} else {
				pruned = append(pruned, "attribute "+e.Name+"."+a.Name)
			}
		}
		var rs []vocab.Relation
		for _, r := range e.Relations {
			if usedAttr[e.Name+"."+r.Name] {
				rs = append(rs, r)
				usedEnt[r.To] = true
			} else {
				pruned = append(pruned, "relation "+e.Name+"."+r.Name)
			}
		}
		e.Attributes, e.Relations = as, rs
		if extension && len(as) == 0 && len(rs) == 0 {
			return e, false
		}
		return e, true
	}
	var ents []vocab.Entity
	for _, e := range p.Extensions {
		if k, ok := keep(e, true); ok {
			out.Extensions = append(out.Extensions, k)
		}
	}
	for _, e := range p.Entities {
		k, _ := keep(e, false)
		ents = append(ents, k)
	}
	for _, e := range ents {
		if usedEnt[e.Name] || len(e.Attributes) > 0 || len(e.Relations) > 0 {
			out.Entities = append(out.Entities, e)
		} else {
			pruned = append(pruned, "entity "+e.Name)
		}
	}
	sort.Strings(pruned)
	return out, pruned
}

// identifier matches an entity, operation or attribute name in rule text.
var identifier = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*`)

// named lists what the rules that do not compile name: entity and operation
// names as written, each attribute as ".name", and each path in full.
func named(cands []translate.Candidate) map[string]bool {
	out := map[string]bool{}
	for _, c := range cands {
		if c.Outcome != "rule" || c.Error == "" {
			continue
		}
		for _, src := range []string{c.Subject, c.On, c.Operation, c.When, c.Assert} {
			for _, id := range identifier.FindAllString(src, -1) {
				out[id] = true
				segs := strings.Split(id, ".")
				out[segs[0]] = true
				for i, seg := range segs[1:] {
					out["."+seg] = true
					out[strings.Join(segs[:i+2], ".")] = true
				}
			}
		}
	}
	return out
}

// ends gives each statement its end state.
func ends(stmts []translate.Statement, cands []translate.Candidate, deferred map[string]string, uncovered, exceeds map[string][]string, blocking map[string]string) ([]End, map[string]int) {
	by := map[string][]translate.Candidate{}
	for _, c := range cands {
		by[c.StatementID] = append(by[c.StatementID], c)
	}
	counts := map[string]int{}
	var out []End
	for _, s := range stmts {
		e := End{ID: s.ID, Source: s.Source}
		var rules, informal, needs, invalid, obligations int
		var reasons []string
		for _, c := range by[s.ID] {
			switch {
			case c.Outcome == "rule" && c.Error == "":
				rules++
				e.Rules = append(e.Rules, c.RuleID)
			case c.Outcome == "rule":
				invalid++
				reasons = append(reasons, c.Error)
			case c.Outcome == "informal":
				informal++
				reasons = append(reasons, c.Rationale)
			case c.Outcome == Obligation:
				obligations++
				reasons = append(reasons, c.Rationale)
			case c.Outcome == "needs_vocabulary":
				needs++
				e.Missing = append(e.Missing, c.MissingTerms...)
				reasons = append(reasons, c.Rationale)
			}
		}
		e.Uncovered = uncovered[s.ID]
		for _, u := range e.Uncovered {
			reasons = append(reasons, fmt.Sprintf("no rule checks %q", u))
		}
		if e.Blocks = blocking[s.ID]; e.Blocks != "" {
			reasons = append(reasons, "blocks a step: "+e.Blocks)
		}
		e.Broader = exceeds[s.ID]
		for _, x := range e.Broader {
			reasons = append(reasons, "broader than the text: "+x)
		}
		switch {
		case invalid > 0:
			e.State = Invalid
		case needs > 0:
			e.State = NeedsVocabulary
		case rules > 0 && informal+obligations+len(e.Uncovered) > 0:
			e.State = Partial
		case rules > 0:
			e.State = Translated
		case obligations > 0:
			e.State = Obligation
		case informal > 0:
			e.State = Informal
		default:
			e.State = Invalid
			if len(by[s.ID]) == 0 {
				reasons = append(reasons, "no translation came back")
			}
		}
		if e.State != Translated || len(e.Broader) > 0 || e.Blocks != "" {
			e.Reason = strings.Join(reasons, " | ")
		}
		if reason, ok := deferred[s.ID]; ok {
			e.State = Invalid
			e.Reason = "deferred for review after repair exhaustion: " + reason
		}
		counts[e.State]++
		out = append(out, e)
	}
	return out, counts
}

func health(reg *ir.Registry, stmts []translate.Statement, rules []parsedRule, es []End) Health {
	h := Health{Statements: len(stmts), Operations: len(reg.Operations)}
	for _, e := range reg.Entities {
		for _, a := range e.Attrs {
			if a.Kind == ir.Enum && len(a.Values) == 0 {
				continue
			}
			h.Attributes++
		}
	}
	for _, e := range es {
		if len(e.Rules) > 0 {
			h.WithRules++
		}
	}
	src := map[string]string{}
	for _, s := range stmts {
		f, _, _ := strings.Cut(s.Source, "#")
		src[s.ID] = f
	}
	readers := map[string]map[string]bool{} // attribute -> statements
	for _, r := range rules {
		for _, a := range r.reads {
			if a.Entity.Name == "Request" || a.Entity.Name == "Actor" || a.Entity.Name == "Result" {
				continue
			}
			q := a.QName()
			if readers[q] == nil {
				readers[q] = map[string]bool{}
			}
			readers[q][r.cand.StatementID] = true
		}
	}
	meets := map[string]bool{}
	for _, ss := range readers {
		if len(ss) > 1 {
			h.SharedAttrs++
		}
		for a := range ss {
			for b := range ss {
				if src[a] != src[b] {
					meets[a] = true
				}
			}
		}
	}
	h.MeetingOtherSrc = len(meets)
	return h
}

func writeReport(dir string, res *Result) error {
	js, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ReportFile), append(js, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, CoverageFile), []byte(Coverage(res)), 0o644)
}

// Coverage renders the end states for the owner: the counts, then every
// statement that is not fully translated, with why.
func Coverage(res *Result) string {
	var b strings.Builder
	h := res.Health
	b.WriteString("# Formalization coverage\n\n")
	fmt.Fprintf(&b, "%d statements: %d translated, %d partial, %d informal, %d need vocabulary, %d invalid.\n\n",
		h.Statements, res.Counts[Translated], res.Counts[Partial], res.Counts[Informal], res.Counts[NeedsVocabulary], res.Counts[Invalid])
	fmt.Fprintf(&b, "Vocabulary: %d attributes, %d operations; %d attributes are read by rules of two or more statements, and %d statements share an attribute with a statement from another source.\n\n",
		h.Attributes, h.Operations, h.SharedAttrs, h.MeetingOtherSrc)
	for i, r := range res.Rounds {
		fmt.Fprintf(&b, "- Round %d: %d statements with %d missing terms; %d symbols added, %d terms mapped to existing ones; %d still need vocabulary.\n", i+1, r.Gaps, r.Missing, r.NewSymbols, r.Mappings, r.After)
	}
	if len(res.Inferred) > 0 {
		fmt.Fprintf(&b, "- %d operation writes inferred from rules about the state after a step.\n", len(res.Inferred))
	}
	for _, d := range res.Defaults {
		fmt.Fprintf(&b, "- %s.\n", d)
	}
	if len(res.Pruned) > 0 {
		fmt.Fprintf(&b, "- %d unused symbols pruned.\n", len(res.Pruned))
	}
	if len(res.Merges) > 0 {
		fmt.Fprintf(&b, "- %d attributes merged into another that names the same fact.\n", len(res.Merges))
	}
	if len(res.Links) > 0 {
		fmt.Fprintf(&b, "- %d world facts proposed in %s, relating two facts no statement relates. Review them: a wrong one invents conflicts.\n", len(res.Links), LinksFile)
	}
	if s := CoverageSummary(res); s != "" {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	if s := SelfCheckSummary(res); s != "" {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	var broader []End
	for _, e := range res.Ends {
		if len(e.Broader) > 0 {
			broader = append(broader, e)
		}
	}
	if len(broader) > 0 {
		fmt.Fprintf(&b, "\n## rules broader than their text (%d)\n\nThese rules forbid something their statement allows, so they can report conflicts the specs do not have.\n\n", len(broader))
		for _, e := range broader {
			fmt.Fprintf(&b, "- **%s** (%s): %s\n", e.ID, e.Source, oneLine(strings.Join(e.Broader, "; "), 300))
		}
	}
	var blocks []End
	for _, e := range res.Ends {
		if e.Blocks != "" {
			blocks = append(blocks, e)
		}
	}
	if len(blocks) > 0 {
		fmt.Fprintf(&b, "\n## rules that block a step (%d)\n\nEach of these rules alone leaves a step no valid outcome, which the check reports as a conflict. That is right when the text forbids the step there (better written as a precondition); otherwise the rule misreads the text.\n\n", len(blocks))
		for _, e := range blocks {
			fmt.Fprintf(&b, "- **%s** (%s): %s\n", e.ID, e.Source, oneLine(e.Blocks, 300))
		}
	}
	for _, st := range []string{NeedsVocabulary, Invalid, Informal, Partial} {
		var rows []End
		for _, e := range res.Ends {
			if e.State == st {
				rows = append(rows, e)
			}
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n\n", st, len(rows))
		for _, e := range rows {
			fmt.Fprintf(&b, "- **%s** (%s)", e.ID, e.Source)
			if len(e.Missing) > 0 {
				fmt.Fprintf(&b, ": missing %s", strings.Join(e.Missing, ", "))
			}
			if e.Reason != "" {
				fmt.Fprintf(&b, ". %s", oneLine(e.Reason, 300))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// consolidateUsed asks which attributes the rules read are one fact under
// two names, merges them, and translates the statements that read a merged
// attribute again, so their rules meet on the target.
func consolidateUsed(ctx context.Context, m llm.Client, stmts []translate.Statement, st *state, opt Options, dir string) error {
	vopt := vocab.Options{VocabFiles: opt.VocabFiles, Context: opt.Context}
	reg, err := vocab.Load(vopt, st.Proposal)
	if err != nil {
		return fmt.Errorf("consolidation: %w", err)
	}
	ours := map[string]bool{}
	for _, e := range append(append([]vocab.Entity{}, st.Proposal.Entities...), st.Proposal.Extensions...) {
		for _, a := range e.Attributes {
			ours[e.Name+"."+a.Name] = true
		}
	}
	prose := map[string]string{}
	for _, s := range stmts {
		prose[s.ID] = s.Prose
	}
	rules := parseRules(reg, st.Cands)
	readers := map[string][]string{} // attribute -> statements
	attrs := map[string]*ir.Attr{}
	var order []string
	for _, r := range rules {
		for _, a := range r.reads {
			q := a.QName()
			if a.Entity.Name == "Request" || a.Entity.Name == "Actor" || a.Entity.Name == "Result" {
				continue
			}
			if readers[q] == nil {
				order = append(order, q)
				attrs[q] = a
			}
			if !slices.Contains(readers[q], r.cand.StatementID) {
				readers[q] = append(readers[q], r.cand.StatementID)
			}
		}
	}
	sort.Strings(order)
	var used []vocab.Used
	for _, k := range opt.Known {
		if readers[k.Name] == nil && reg.Entities[strings.Split(k.Name, ".")[0]] != nil {
			used = append(used, k)
		}
	}
	for _, q := range order {
		a := attrs[q]
		u := vocab.Used{Name: q, Type: typeOf(a), Meaning: a.Meaning}
		for k, id := range readers[q] {
			if k < 2 {
				u.Examples = append(u.Examples, oneLine(prose[id], 160))
			}
			u.Text += " " + prose[id]
		}
		used = append(used, u)
	}
	pairs := append(vocab.SimilarPairs(used, 4, 0.15), coAsserted(rules, used)...)
	var mine [][2]int // a merge needs at least one side the proposal owns
	for _, p := range pairs {
		if ours[used[p[0]].Name] || ours[used[p[1]].Name] {
			mine = append(mine, p)
		}
	}
	fmt.Fprintf(opt.Log, "consolidate: %d attribute(s) read by rules, %d candidate pair(s) ...\n", len(used), len(mine))
	vrep := &vocab.Report{}
	merges, links, err := vocab.ConsolidatePairs(ctx, m, reg, used, mine, 60, opt.Parallel, vrep)
	if err == nil && opt.FactMap {
		// Synonyms whose names share nothing are never offered as a pair:
		// ask once over the whole list too (ISSUES 16, 17).
		setBy := map[string]string{}
		for q, a := range attrs {
			var ws []string
			for _, op := range reg.Operations {
				if op.WritesAttr(a) {
					ws = append(ws, op.Name)
				}
			}
			sort.Strings(ws)
			switch {
			case a.WorldSet:
				setBy[q] = "the world"
			case len(ws) > 0:
				setBy[q] = "written by " + strings.Join(ws, ", ")
			default:
				setBy[q] = "written by no step"
			}
		}
		var fm []vocab.Merge
		var fl []vocab.Link
		if fm, fl, err = vocab.FactMap(ctx, m, reg, used, setBy, vrep); err == nil {
			fmt.Fprintf(opt.Log, "consolidate: the fact map proposed %d merge(s) and %d link(s)\n", len(fm), len(fl))
			for _, mg := range fm {
				if !slices.ContainsFunc(merges, func(x vocab.Merge) bool { return x.From == mg.From }) {
					merges = append(merges, mg)
				}
			}
			links = append(links, fl...)
		}
	}
	st.Usage.Add(vrep.Usage)
	st.Errors = append(st.Errors, vrep.Errors...)
	if err != nil {
		st.Errors = append(st.Errors, "consolidation: "+err.Error())
		return nil
	}
	st.Links = nil
	for i, l := range links {
		st.Links = append(st.Links, translate.Candidate{StatementID: LinkPrefix + fmt.Sprint(i+1), RuleID: LinkPrefix + fmt.Sprint(i+1),
			Outcome: "rule", Kind: ir.Invariant, Effect: "none", When: l.When, Assert: l.Assert,
			Rationale: fmt.Sprintf("%s (between %s and %s)", l.Reason, l.A, l.B)})
	}
	if opt.Compact {
		return compactAliases(st, merges, opt, dir)
	}
	// Keep the merges whose source is ours and whose target is declared,
	// one at a time, each only if the vocabulary still loads.
	p := st.Proposal
	var applied []vocab.Merge
	for _, mg := range merges {
		ent, an, _ := strings.Cut(mg.Into, ".")
		if !ours[mg.From] || reg.Entities[ent] == nil || reg.Entities[ent].Attrs[an] == nil {
			continue
		}
		next, ok := vocab.ApplyMerges(p, []vocab.Merge{mg})
		if len(ok) == 0 {
			continue
		}
		if _, err := vocab.Load(vopt, next); err != nil {
			continue
		}
		p = next
		applied = append(applied, ok...)
	}
	fmt.Fprintf(opt.Log, "consolidate: %d merge(s) proposed, %d applied; %d link(s)\n", len(merges), len(applied), len(st.Links))
	if len(applied) == 0 {
		return nil
	}
	st.Merges = append(st.Merges, applied...)
	st.Proposal = p
	files, err := writeVocabulary(dir, p, opt)
	if err != nil {
		return err
	}
	gone := map[string]vocab.Merge{}
	for _, mg := range applied {
		gone[mg.From] = mg
	}
	var again []translate.Statement
	seen := map[string]bool{}
	for _, s := range stmts {
		for _, r := range rules {
			if r.cand.StatementID != s.ID || seen[s.ID] {
				continue
			}
			for _, a := range r.reads {
				if mg, ok := gone[a.QName()]; ok {
					seen[s.ID] = true
					s.Notes = append(st.Notes[s.ID], fmt.Sprintf("%s was merged into %s (%s)", mg.From, mg.Into, mg.Reason))
					again = append(again, s)
					break
				}
			}
		}
	}
	fmt.Fprintf(opt.Log, "consolidate: translating %d statement(s) again ...\n", len(again))
	rep, err := translate.Translate(ctx, []llm.Client{m}, again, translate.Options{VocabFiles: files, MaxRepairs: opt.MaxRepairs, BatchSize: opt.Batch, Parallel: opt.Parallel})
	if err != nil {
		return fmt.Errorf("consolidation translate: %w", err)
	}
	st.Usage.Add(rep.Usage)
	st.Errors = append(st.Errors, rep.BatchErrors...)
	st.Cands = replace(st.Cands, rep.Candidates)
	return nil
}

func typeOf(a *ir.Attr) string {
	switch a.Kind {
	case ir.Int:
		return fmt.Sprintf("int %d..%d", a.Min, a.Max)
	case ir.Enum:
		return "enum {" + strings.Join(a.Values, ", ") + "}"
	}
	return a.TypeString()
}

// LinkPrefix starts the IDs of links (world facts found by consolidation).
const LinkPrefix = "link-"

// coAsserted pairs attributes that rules from different statements assert
// after the same operation: where requirements about one step meet, facts
// that constrain each other are likely.
func coAsserted(rules []parsedRule, used []vocab.Used) [][2]int {
	at := map[string]int{}
	for i, u := range used {
		at[u.Name] = i
	}
	type ref struct {
		attr int
		stmt string
	}
	byOp := map[string][]ref{}
	for _, r := range rules {
		if r.cand.On == "" {
			continue
		}
		for _, a := range r.after {
			if i, ok := at[a.QName()]; ok {
				byOp[r.cand.On] = append(byOp[r.cand.On], ref{i, r.cand.StatementID})
			}
		}
	}
	ops := make([]string, 0, len(byOp))
	for op := range byOp {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	seen := map[[2]int]bool{}
	var out [][2]int
	for _, op := range ops {
		rs := byOp[op]
		for x := range rs {
			for y := x + 1; y < len(rs); y++ {
				a, b := rs[x], rs[y]
				if a.stmt == b.stmt || a.attr == b.attr {
					continue
				}
				p := [2]int{min(a.attr, b.attr), max(a.attr, b.attr)}
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// writeLinks writes the links that compile, as world facts for the owner
// to review, and returns their IDs.
func writeLinks(dir string, links []translate.Candidate) ([]string, error) {
	f := filepath.Join(dir, LinksFile)
	var b strings.Builder
	var ids []string
	for _, l := range links {
		if l.Error != "" {
			continue
		}
		ids = append(ids, l.RuleID)
		fmt.Fprintf(&b, "---\nid: %s\nstatus: active\nkind: invariant\n", l.RuleID)
		if l.When != "" {
			fmt.Fprintf(&b, "when: %q\n", l.When)
		}
		fmt.Fprintf(&b, "assert: %q\nsource:\n  inferredBy: stele formalize\n  prose: %q\n", l.Assert, "World fact: "+l.Rationale)
	}
	if len(ids) == 0 {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return nil, nil
	}
	header := "# World facts proposed by `stele formalize`: how two things relate by their nature, which no\n# statement says. Review each: keep, edit or delete. A wrong one invents conflicts.\n"
	return ids, os.WriteFile(f, []byte(header+b.String()), 0o644)
}

// resolveOverrides turns an override that names a requirement by the label
// its text uses ("FR-005") into that statement's ID: the statement of the
// same document whose ID or text starts with the label, when exactly one
// does. An override that names no statement is dropped.
func resolveOverrides(stmts []translate.Statement, cands []translate.Candidate) {
	ids := map[string]bool{}
	doc := map[string]string{}
	for _, s := range stmts {
		ids[s.ID] = true
		doc[s.ID], _, _ = strings.Cut(s.Source, "#")
	}
	for i := range cands {
		c := &cands[i]
		var kept []string
		for _, ref := range c.Overrides {
			if ids[ref] {
				kept = append(kept, ref)
				continue
			}
			label := strings.Trim(ref, " []*")
			var found []string
			for _, s := range stmts {
				if s.ID == c.StatementID || doc[s.ID] != doc[c.StatementID] {
					continue
				}
				_, tail, _ := strings.Cut(s.ID, "/")
				if s.ID == label || tail == label || strings.HasSuffix(s.ID, "#"+label) || strings.HasPrefix(s.Prose, label+":") {
					found = append(found, s.ID)
				}
			}
			if len(found) == 1 && !slices.Contains(kept, found[0]) {
				kept = append(kept, found[0])
			}
		}
		c.Overrides = kept
	}
}

var increment = regexp.MustCompile(`([\w.]+)\s*=\s*\(?\s*old\(\s*([\w.]+)\s*\)\s*\+`)

// unboundCounters drops the maximum of an int attribute a rule increments
// (x = old(x) + k): a counter has no maximum unless a statement states
// one, and an invented maximum makes the increment impossible at it, which
// the checker would report as a conflict.
func unboundCounters(p vocab.Proposal, rules []parsedRule) vocab.Proposal {
	counters := map[string]bool{}
	for _, r := range rules {
		for _, m := range increment.FindAllStringSubmatch(r.cand.Assert, -1) {
			if m[1] != m[2] {
				continue
			}
			name := m[1][strings.LastIndex(m[1], ".")+1:]
			for _, a := range r.after {
				if a.Name == name && a.Kind == ir.Int && a.HasRange {
					counters[a.QName()] = true
				}
			}
		}
	}
	if len(counters) == 0 {
		return p
	}
	for _, es := range []*[]vocab.Entity{&p.Entities, &p.Extensions} {
		*es = slices.Clone(*es)
		for i := range *es {
			(*es)[i].Attributes = slices.Clone((*es)[i].Attributes)
			for k := range (*es)[i].Attributes {
				a := &(*es)[i].Attributes[k]
				if counters[(*es)[i].Name+"."+a.Name] && strings.EqualFold(a.Type, "int") {
					a.Min, a.Max = 0, 0
				}
			}
		}
	}
	return p
}

// applyLessons adds each statement's relevant lessons to its notes and
// returns which lessons each statement got.
func applyLessons(stmts []translate.Statement, ls []*ir.Lesson) map[string][]string {
	if len(ls) == 0 {
		return nil
	}
	out := map[string][]string{}
	for i := range stmts {
		s := &stmts[i]
		s.Notes = slices.Clone(s.Notes) // the caller's statements keep their notes
		for _, l := range lesson.Relevant(ls, s.ID, translate.TextHash(s.Prose), s.Prose, 8) {
			if n := lesson.Note(l); !slices.Contains(s.Notes, n) { // a caller may have added it already
				s.Notes = append(s.Notes, n)
			}
			out[s.ID] = append(out[s.ID], l.ID)
		}
	}
	return out
}
