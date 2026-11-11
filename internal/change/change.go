// Package change checks one spec change as a unit before it is applied
// (D-057): an OpenSpec change folder's added, modified, removed and renamed
// requirements are applied to the current statements, only the statements
// the change adds or edits are translated, over the registry's vocabulary
// plus the terms the change needs, and the gate checks the registry the
// change would produce against the one it starts from. What needs deciding
// comes back as questions in the spec's words, for the coding agent to put
// to the owner (D-058).
package change

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/formalize"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lesson"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

// Options name the change and where its inputs are.
type Options struct {
	Repo     string // the project's working tree
	Change   string // the change folder's name under openspec/changes
	Registry string // the approved registry
	Out      string // work directory: translated/, head/
	// Truth is the statement set the registry was built from. Empty means
	// a fresh import of openspec/specs.
	Truth []translate.Statement
	// VocabFiles are given to the translator. Empty means every registry
	// file that declares no statement rule.
	VocabFiles []string
	// Base, when set, makes the change the difference between the specs at
	// this git ref and the working tree, read in Format (speckit, kiro or
	// openspec): a Spec Kit or Kiro feature branch, for example. Empty
	// means an OpenSpec change folder.
	Base   string
	Format string
	// Adopt, when set, makes the change the adoption of these statements,
	// the project's whole specs, into an empty registry: every statement
	// is new. SpecsDir is where they are written, for the approval's
	// fingerprint.
	Adopt []translate.Statement
	// AdoptName names the part of the specs Adopt holds, when the specs
	// are adopted a part at a time: its registry files carry the name.
	AdoptName string
	SpecsDir  string
	// AnyCase is the Markdown importer's: lower-case must and should count.
	AnyCase bool
	Clients []llm.Client
	Z3      *solver.Z3
	Log     io.Writer
	// NoCode skips asking what the code does for each conflict (D-062);
	// Graph also finds definitions in Grove's code graph (no gain measured
	// on 2026-10-29, so off by default).
	NoCode, Graph bool
	// MaxCost caps the translation's model calls, in dollars as the backend
	// reports them (formalize.Options.MaxCost); 0 is unlimited.
	MaxCost float64
	// RequestTimeout limits each model call (formalize.Options); a call
	// that stalls fails and is retried, instead of using up the whole run.
	RequestTimeout time.Duration

	// NoReview skips an adoption's review of its texts for contradictions
	// the rules do not show (crosscheck.go).
	NoReview bool
	// NoAudit skips the check of a conflict's rules against their texts.
	NoAudit bool
	// steerFirst: the texts are being reviewed before any reading, so the
	// review's findings steer the first one.
	steerFirst bool
	settles    int          // times Stele's own answers sent the check round again
	rounds     int          // times the glossary's definitions added facts this run
	lessons    []*ir.Lesson // the registry's lessons, for the coverage judge
}

// request is ctx limited to one model call's RequestTimeout.
func (o Options) request(ctx context.Context) (context.Context, context.CancelFunc) {
	if o.RequestTimeout > 0 {
		return context.WithTimeout(ctx, o.RequestTimeout)
	}
	return ctx, func() {}
}

// Result is what the change does to the registry and what the gate found.
type Result struct {
	Change string `json:"change"`
	// Format is the spec format the change is read in (empty for OpenSpec
	// change folders and adoption).
	Format   string            `json:"format,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
	Added    []string          `json:"added,omitempty"`
	Modified []string          `json:"modified,omitempty"`
	Removed  []string          `json:"removed,omitempty"`
	Renamed  map[string]string `json:"renamed,omitempty"`
	// Statements maps every statement ID the change touches to its text
	// after the change, and Sources to where it is written.
	Statements map[string]string `json:"-"`
	// Texts are the texts of the statements the change adds or edits.
	Texts   map[string]string `json:"texts,omitempty"`
	Sources map[string]string `json:"-"`
	// Rules maps each translated statement to the rules it became;
	// Unchecked lists those that became no checked rule, with the reason.
	Rules     map[string][]string `json:"rules,omitempty"`
	Unchecked map[string]string   `json:"unchecked,omitempty"`
	// Findings maps a translated statement to what the coverage and self
	// checks found in its reading (formalize: cover.go, selfcheck.go).
	Findings map[string][]string `json:"findings,omitempty"`
	// Lessons maps a translated statement to the owner's lessons it was
	// read with; NewLessons are the lessons this change's answers add to
	// the registry, committed with it (D-063).
	Lessons    map[string][]string `json:"lessons,omitempty"`
	NewLessons []string            `json:"new_lessons,omitempty"`
	Replaced   []string            `json:"replaced,omitempty"` // rules given a new version
	Retired    []string            `json:"retired,omitempty"`
	// RenameGuesses are removed and added requirements whose texts are
	// alike: likely one requirement rewritten under a new title without a
	// RENAMED record. The owner says; "same" makes it a rename.
	RenameGuesses []RenameGuess `json:"rename_guesses,omitempty"`
	Head          string        `json:"head"`
	Gate          *gate.Result  `json:"gate"`
	// Open counts the conflicts the owner left open at this adoption.
	Open int `json:"open,omitempty"`
	// AdoptSpecs are the specs this adoption reads, when the project's
	// specs are adopted a part at a time, and UnreadSpecs those no
	// adoption has read yet.
	AdoptSpecs  []string `json:"adopt_specs,omitempty"`
	UnreadSpecs []string `json:"unread_specs,omitempty"`
	// Possible are the contradictions a review of the texts found that
	// the rules do not show, after their requirements were read again.
	Possible []Possible `json:"possible_conflicts,omitempty"`
	// SettledOpen, SettledPrecedence and SettledUnchecked count what Stele
	// settled itself at an adoption (settle.go): clashes of its rules that
	// a reading of the texts does not show, recorded as open or settled by
	// precedence, and requirements recorded without rules.
	SettledOpen       int `json:"settled_open,omitempty"`
	SettledPrecedence int `json:"settled_precedence,omitempty"`
	SettledUnchecked  int `json:"settled_unchecked,omitempty"`
	// FlagOnly are the requirements whose only rules state one yes/no fact
	// nothing else reads: recorded, and checked against nothing yet.
	FlagOnly []string `json:"flag_only,omitempty"`
	// SetAside counts the rules of the change set aside because they
	// clashed with another requirement and did not say what their own
	// text says (audit.go).
	SetAside int       `json:"set_aside,omitempty"`
	Usage    llm.Usage `json:"usage"`
	// Reused lists the statements whose earlier translation was kept: their
	// text and notes are unchanged.
	Reused []string `json:"reused,omitempty"`
	// NewTerms are the symbols the change adds to the vocabulary; SameTerms
	// the new names Stele folded into an existing symbol by itself.
	NewTerms  []string      `json:"new_terms,omitempty"`
	SameTerms []vocab.Merge `json:"same_terms,omitempty"`
	// InferredWrites are what the change's rules need existing steps to
	// write: operation -> attributes.
	InferredWrites map[string][]string `json:"inferred_writes,omitempty"`
	// SystemSet are the change's own world-set terms its rules fix after a
	// step, made system-set: operation -> attributes.
	SystemSet map[string][]string `json:"system_set,omitempty"`
	Questions []Question          `json:"questions"`
	// Fingerprint identifies the change folder and the registry checked, so
	// an approval can tell the check is still current.
	Fingerprint string `json:"fingerprint"`
	// Approved: the registry already holds the owner's approval of every
	// requirement the change adds or edits, as it now reads.
	Approved bool `json:"approved,omitempty"`
	// Failed are the requirements the model could not translate (a backend
	// error, or no valid reading after the repairs), with why. Nothing was
	// checked for them, so the change can be neither clean nor approved
	// until a later check translates them; failures are never cached.
	Failed  map[string]string `json:"failed,omitempty"`
	Watched []string          `json:"watched"` // the roots the fingerprint covers
	// HeadFingerprint covers Head, the rules the check wrote: the approval
	// applies exactly those.
	HeadFingerprint string `json:"head_fingerprint,omitempty"`
	Repo            string `json:"repo"`
	Registry        string `json:"registry"`
	// narrow maps a pair of conflicting statements to the one whose
	// condition lies inside the other's.
	narrow map[string]string
	// creators names, by conflict (conflictKey), the operations that,
	// recorded as creating their entity, would set the conflict aside.
	creators map[string][]string
	// reportedWrites names, by operation, what the step is recorded as
	// writing that its approved rules only report (formalize.OnlyReported).
	reportedWrites map[string][]string
	// first: the registry held no approved rule before this change.
	first bool
}

// Run checks the change.
func Run(ctx context.Context, opt Options) (*Result, error) {
	res := &Result{Change: opt.Change, Format: opt.Format, Rules: map[string][]string{}, Unchecked: map[string]string{},
		Statements: map[string]string{}, Sources: map[string]string{}}
	var truth, after []translate.Statement
	var err error
	if opt.Format == "shale" && opt.Adopt == nil {
		if reg, err := ir.LoadDir(opt.Registry); err == nil {
			if err := importer.KeepApproved(opt.Repo, approvedStatements(reg)); err != nil {
				return nil, err
			}
		}
	}
	// What the check reads is fingerprinted before it is read, and again
	// when the check ends: a requirement edited while the check ran was
	// checked as it read before, and an approval that compared the files
	// with a fingerprint taken afterwards approved the new text under the
	// old reading (a code review, 2026-11-11).
	switch {
	case opt.Adopt != nil, opt.Format == "markdown":
		res.Watched = []string{opt.SpecsDir}
	case opt.Base == "":
		res.Watched = []string{filepath.Join(opt.Repo, "openspec", "changes", opt.Change)}
	default:
		res.Watched = []string{filepath.Join(opt.Repo, specRoot(opt.Format))}
	}
	res.Watched = append(res.Watched, opt.Registry)
	read, err := Fingerprint(res.Watched...)
	if err != nil {
		return nil, err
	}
	switch {
	case opt.Adopt != nil:
		after = opt.Adopt
		for _, s := range after {
			res.Added = append(res.Added, s.ID)
		}
	case opt.Format == "markdown":
		// Plain Markdown in the folder stele.yaml names (SpecsDir).
		if truth, after, err = markdownChange(opt, res); err != nil {
			return nil, err
		}
	case opt.Base == "":
		if truth, after, err = openSpecChange(opt, res); err != nil {
			return nil, err
		}
	default:
		if truth, after, err = branchChange(opt, res); err != nil {
			return nil, err
		}
	}
	res.Texts = map[string]string{}
	for _, s := range after {
		if slices.Contains(res.Added, s.ID) || slices.Contains(res.Modified, s.ID) {
			res.Texts[s.ID] = s.Prose
		}
	}
	for _, s := range append(append([]translate.Statement{}, truth...), after...) {
		res.Statements[s.ID], res.Sources[s.ID] = s.Prose, s.Source
	}

	base, err := ir.LoadDir(opt.Registry)
	if err != nil {
		return nil, fmt.Errorf("registry %s: %w", opt.Registry, err)
	}
	if len(res.Removed) == 0 && approvedAlready(base, res.Texts) {
		// The owner approved this change as it reads: checking it against a
		// registry that already holds it would compare it with itself.
		res.Approved = true
		res.Gate = &gate.Result{Accepted: true}
		return res, nil
	}
	opt.lessons = base.Lessons
	res.first = !slices.ContainsFunc(base.Rules, func(x *ir.Rule) bool { return x.Live() && x.Approved() })
	answers, err := LoadAnswers(opt.Out)
	if err != nil {
		return nil, err
	}
	// Renames: the change's own records, plus the owner saying that a
	// requirement removed and one added are the same, rewritten.
	renamed := map[string]string{}
	for from, to := range res.Renamed {
		renamed[from] = to
	}
	for _, a := range answers {
		if a.Kind == "renamed" && a.Choice == "same" && a.Term != "" && a.Existing != "" {
			renamed[a.Term] = a.Existing
			res.Removed = slices.DeleteFunc(res.Removed, func(id string) bool { return id == a.Term })
			if i := slices.Index(res.Added, a.Existing); i >= 0 {
				res.Added = slices.Delete(res.Added, i, i+1)
				if !slices.Contains(res.Modified, a.Existing) {
					res.Modified = append(res.Modified, a.Existing)
				}
			}
		}
	}
	// A request the owner retired for the one that restates it (an
	// exception or update_old answer) is that request, edited: asking
	// "the same requirement?" after the owner said so was one more turn on
	// every restatement (a developer trial, 2026-11-03).
	if opt.Format == "shale" {
		rs, _ := importer.RetiredIntents(opt.Repo)
		for _, r := range rs {
			if r.For == "" || renamed[r.ID] != "" || !slices.Contains(res.Removed, r.ID) {
				continue
			}
			var to []string
			for _, f := range strings.Split(r.For, ", ") {
				// Restated, it reads alike; a different requirement that
				// replaced it is not the same one.
				if slices.Contains(res.Added, f) && !slices.Contains(slices.Collect(maps.Values(renamed)), f) &&
					adopt.Similarity(res.Statements[r.ID], res.Statements[f]) >= renameGuessThreshold {
					to = append(to, f)
				}
			}
			if len(to) != 1 || slices.ContainsFunc(answers, func(a Answer) bool {
				return a.Kind == "renamed" && a.Term == r.ID && a.Existing == to[0] && a.Choice == "different"
			}) {
				continue
			}
			renamed[r.ID] = to[0]
			res.Removed = slices.DeleteFunc(res.Removed, func(id string) bool { return id == r.ID })
			if i := slices.Index(res.Added, to[0]); i >= 0 {
				res.Added = slices.Delete(res.Added, i, i+1)
				res.Modified = append(res.Modified, to[0])
			}
		}
	}
	if len(renamed) > 0 {
		res.Renamed = renamed
	}
	rep := adopt.SyncRenamed(truth, after, base, renamed)
	res.RenameGuesses = guessRenames(rep, res.Statements, answers)
	if len(rep.Ambiguous) > 0 {
		for id, to := range rep.Ambiguous {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s matches several statements after the change (%s); identify it before applying", id, strings.Join(to, ", ")))
		}
	}
	want := map[string]bool{}
	for _, id := range append(append([]string{}, rep.Added...), rep.Changed...) {
		want[id] = true
	}
	var pending []translate.Statement
	for _, s := range after {
		if want[s.ID] {
			pending = append(pending, s)
		}
	}

	// An adoption reviews its texts first, so that what the review sees
	// steers the first reading.
	if opt.Adopt != nil && !opt.NoReview && len(opt.Clients) > 0 && len(pending) > 0 {
		first := opt
		first.steerFirst = !dirHasReadings(opt.Out)
		reviewTexts(ctx, first, res, base)
	}
	// Translate what the change adds or edits, over the registry's
	// vocabulary and the terms the change proposes.
	for i := range pending {
		pending[i].Notes = append(pending[i].Notes, notes(answers, pending[i])...)
		pending[i].Notes = append(pending[i].Notes, crossNotes(opt.Out, pending[i])...)
		// What the owner taught Stele earlier (D-063); in the notes, so the
		// translation cache notices a changed lesson.
		for _, l := range lesson.Relevant(base.Lessons, pending[i].ID, translate.TextHash(pending[i].Prose), pending[i].Prose, 8) {
			pending[i].Notes = append(pending[i].Notes, lesson.Note(l))
			if res.Lessons == nil {
				res.Lessons = map[string][]string{}
			}
			res.Lessons[pending[i].ID] = append(res.Lessons[pending[i].ID], l.ID)
		}
	}
	vocabFiles := opt.VocabFiles
	if len(vocabFiles) == 0 {
		if vocabFiles, err = VocabFiles(opt.Registry, base); err != nil {
			return nil, err
		}
	}
	newIn := contextOf(base)
	digest, err := digestFiles(vocabFiles, newIn)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opt.Out, 0o755); err != nil {
		return nil, err
	}
	c, err := loadCache(opt.Out, digest)
	if err != nil {
		return nil, err
	}
	for _, a := range answers {
		switch {
		case a.Kind == "same_term" && a.Choice == "same":
			c.Proposal = dropAttr(c.Proposal, a.Term, a.Existing)
		case a.Kind == "conflict" && a.Choice == "wrong_fact":
			c.Links = slices.DeleteFunc(c.Links, func(l translate.Candidate) bool { return rejectedFact([]Answer{a}, l.RuleID, candFactKey(l)) })
		}
	}
	known := knownUses(base, res.Statements)
	var docs []*yaml.Node
	var linkCands []translate.Candidate
	vocabFile := ""
	var checkFiles []string // the vocabulary a world fact is checked against
	if len(pending) > 0 {
		var todo []translate.Statement
		for _, s := range pending {
			// An entry with no reading is a failure an earlier release cached.
			if e, ok := c.Entries[s.ID]; ok && e.Key == key(s) && len(e.Cands) > 0 {
				res.Reused = append(res.Reused, s.ID)
			} else {
				todo = append(todo, s)
			}
		}
		if len(todo) > 0 {
			if err := c.translate(ctx, opt, todo, vocabFiles, newIn, known, res); err != nil {
				return nil, err
			}
		}
		var all []translate.Candidate
		for _, s := range pending {
			if leftUnchecked(answers, s.ID, translate.TextHash(res.Statements[s.ID])) {
				// Read twice, its rules contradicted each other both times,
				// and the owner chose to record it without rules.
				all = append(all, translate.Candidate{StatementID: s.ID, Outcome: "informal", Rationale: "Stele's readings of it contradicted themselves; recorded without rules at the owner's word"})
				continue
			}
			all = append(all, c.Entries[s.ID].Cands...)
			if f := c.Entries[s.ID].Findings; len(f) > 0 {
				if res.Findings == nil {
					res.Findings = map[string][]string{}
				}
				res.Findings[s.ID] = f
			}
		}
		keepRuleIDs(all, rep.RulesOf, base)
		all = setAside(all, loadAudit(opt.Out), c, res)
		textPrecedence(all, res.Statements)
		answerPrecedence(all, answers, res.Statements)
		p := prune(c.Proposal, all)
		res.NewTerms = terms(p)
		for _, mg := range c.Merges {
			if !slices.ContainsFunc(res.SameTerms, func(x vocab.Merge) bool { return x.From == mg.From }) {
				res.SameTerms = append(res.SameTerms, mg)
			}
		}
		dir := filepath.Join(opt.Out, "translated")
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		files := vocabFiles
		writeVocab := func(p vocab.Proposal) error {
			src, err := vocab.YAML(p, newIn)
			if err != nil || len(src) == 0 {
				return err
			}
			vocabFile = filepath.Join(dir, "change-vocabulary.yaml")
			files = append(append([]string{}, vocabFiles...), vocabFile)
			return os.WriteFile(vocabFile, append([]byte("# Terms this change adds, proposed by `stele change`.\n"), src...), 0o644)
		}
		if err := writeVocab(p); err != nil {
			return nil, err
		}
		if reg, err := ir.LoadFiles(files...); err == nil {
			var flipped map[string][]string
			if p, flipped = systemSet(reg, p, all); len(flipped) > 0 {
				res.SystemSet = flipped
				if err := writeVocab(p); err != nil {
					return nil, err
				}
			}
		}
		// A step proposed on an earlier check writes what this check's rules
		// set after it: the resend of a stalled model call read as keeping
		// the time limit a later request gave it, and clashed (Stele's own
		// change, 2026-11-05).
		if reg, err := ir.LoadFiles(files...); err == nil {
			if q, added := formalize.InferWrites(reg, p, all); len(added) > 0 {
				p = q
				if err := writeVocab(p); err != nil {
					return nil, err
				}
			}
		}
		translate.Revalidate(files, all)
		if reg, err := ir.LoadFiles(files...); err == nil {
			res.InferredWrites = inferWrites(reg, p, all)
		}
		// A reading made after the owner's answer may propose a dropped
		// fact again, under a new ID.
		c.Links = slices.DeleteFunc(c.Links, func(l translate.Candidate) bool { return rejectedFact(answers, l.RuleID, candFactKey(l)) })
		linkCands = append(linkCands, c.Links...)
		translate.Revalidate(files, linkCands)
		checkFiles = files
		var again bool
		if all, again = c.settleInvalid(all, pending, res); again {
			// A finished run is read back, not made again: start afresh.
			if err := os.RemoveAll(filepath.Join(opt.Out, "runs")); err != nil {
				return nil, err
			}
		}
		if err := translate.WriteRegistry(dir, &translate.Report{Backend: "stele change", Statements: len(pending), Candidates: all, Inputs: pending}, pending, nil); err != nil {
			return nil, err
		}
		reasons := map[string]string{}
		for _, c := range all {
			switch {
			case c.Error != "":
				reasons[c.StatementID] = "invalid: " + c.Error
				unusable(res, c)
			case c.Outcome == "rule" && !c.Draft:
				res.Rules[c.StatementID] = append(res.Rules[c.StatementID], c.RuleID)
			case c.Outcome == "rule":
				reasons[c.StatementID] = "readings disagree (see translated/questions.md)"
			case c.Outcome == "needs_vocabulary":
				reasons[c.StatementID] = "needs vocabulary: " + strings.Join(c.MissingTerms, ", ")
			default:
				reasons[c.StatementID] = c.Outcome + ": " + c.Rationale
			}
		}
		// A statement with at least one checked rule is checked, if only in
		// part; the others say nothing about it.
		for id, why := range reasons {
			if len(res.Rules[id]) == 0 {
				res.Unchecked[id] = why
			}
		}
		if docs, err = readDocs(filepath.Join(dir, "rules.yaml")); err != nil {
			return nil, err
		}
	}
	if err := c.save(opt.Out); err != nil {
		return nil, err
	}

	// The head: the registry as the change leaves it.
	res.Head = filepath.Join(opt.Out, "head")
	if err := os.RemoveAll(res.Head); err != nil {
		return nil, err
	}
	if err := copyTree(opt.Registry, res.Head); err != nil {
		return nil, err
	}
	for _, l := range lessonsFrom(answers, opt.Change) {
		id, added, err := lesson.Add(res.Head, l)
		if err != nil {
			return nil, fmt.Errorf("lesson from %s: %w", l.From, err)
		}
		if added {
			res.NewLessons = append(res.NewLessons, id+": "+l.Says)
		}
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		return nil, err
	}
	for _, p := range lesson.Contradictions(head.Lessons) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("lessons %s and %s concern the same terms and say different things: %q, %q; remove the wrong one with `stele lesson remove`", p[0].ID, p[1].ID, p[0].Says, p[1].Says))
	}
	newIDs := map[string]bool{}
	for _, doc := range docs {
		id := scalar(doc, "id")
		newIDs[id] = true
		if b := base.Rule(id); b != nil {
			setScalar(doc, "version", fmt.Sprint(b.Version+1))
			res.Replaced = append(res.Replaced, id)
		}
	}
	if len(res.Replaced) > 0 {
		if err := adopt.RemoveRules(head, res.Replaced); err != nil {
			return nil, err
		}
	}
	// The registry files this change writes; an adoption's are the
	// project's founding requirements, not a change.
	prefix := RegistryPrefix(opt.Change)
	if opt.Adopt != nil {
		prefix = "adopted"
		if opt.AdoptName != "" {
			prefix += "-" + opt.AdoptName // one part of the specs, adopted on its own
		}
	}
	if len(docs) > 0 {
		if err := writeDocs(filepath.Join(res.Head, prefix+".yaml"), docs); err != nil {
			return nil, err
		}
	}
	if vocabFile != "" {
		if err := copyFile(vocabFile, filepath.Join(res.Head, prefix+".vocabulary.yaml")); err != nil {
			return nil, err
		}
	}
	if err := writeLinks(filepath.Join(res.Head, prefix+".world.yaml"), linkCands); err != nil {
		return nil, err
	}
	if err := writeWrites(filepath.Join(res.Head, prefix+".writes.yaml"), res.InferredWrites, keeps(answers)); err != nil {
		return nil, err
	}
	// An answer about a step the current reading no longer has is left out:
	// a reread named the entity differently, and the answer's "extend Plot"
	// stopped the change's registry from loading (a developer trial on
	// Codex, 2026-11-06). Stele asks again where the step still matters.
	created := creators(answers)
	if reg, err := ir.LoadDir(res.Head); err == nil {
		for e, ops := range created {
			if reg.Entities[e] == nil {
				delete(created, e)
				continue
			}
			created[e] = slices.DeleteFunc(ops, func(op string) bool { return reg.Operations[op] == nil })
			if len(created[e]) == 0 {
				delete(created, e)
			}
		}
	}
	if err := writeCreators(filepath.Join(res.Head, prefix+".created.yaml"), created, newIn); err != nil {
		return nil, err
	}
	retire := map[string]bool{}
	for _, id := range rep.Retire {
		retire[id] = true
	}
	for _, pins := range rep.RulesOf {
		for _, p := range pins {
			id, _, _ := strings.Cut(p, "@")
			if !newIDs[id] {
				retire[id] = true
			}
		}
	}
	// A world fact approved with an earlier change that the owner now says
	// is wrong: the answer could only drop the change's own facts, so a
	// conflict resting on an approved one came back however it was
	// answered (a developer trial, 2026-11-03).
	for _, x := range base.Rules {
		if x.Live() && x.StatementID == "" && strings.HasPrefix(x.ID, "world-") && rejectedFact(answers, x.ID, ruleFactKey(x)) {
			retire[x.ID] = true
		}
	}
	for id := range retire {
		if x := base.Rule(id); x != nil && x.Live() {
			res.Retired = append(res.Retired, id)
		}
	}
	sort.Strings(res.Retired)
	sort.Strings(res.Replaced)
	if head, err = ir.LoadDir(res.Head); err != nil {
		return nil, fmt.Errorf("the registry after the change does not load: %w", err)
	}
	if len(res.Retired) > 0 {
		if err := adopt.RetireRules(head, res.Retired); err != nil {
			return nil, err
		}
	}
	if len(rep.Sources) > 0 {
		if head, err = ir.LoadDir(res.Head); err != nil {
			return nil, err
		}
		if err := adopt.UpdateSources(head, rep.Sources); err != nil {
			return nil, err
		}
	}
	z := opt.Z3
	if z == nil {
		z = &solver.Z3{}
	}
	if res.Gate, err = gate.Dirs(ctx, z, opt.Registry, res.Head); err != nil {
		return nil, err
	}
	if head, err = ir.LoadDir(res.Head); err != nil {
		head = base
	}
	if res.Change == Adoption && res.Gate.Check != nil {
		// Conflicts the owner chose to decide later are recorded as open,
		// and the check is run again without them.
		var all []adopt.LeftOpen
		for round := 0; round < 3; round++ {
			// Known conflicts can bring out the same requirements' clash in
			// another form: decided later too.
			open := leftOpen(res, head, answers)
			if len(open) == 0 {
				break
			}
			all = append(all, open...)
			if err := adopt.WriteOpen(res.Head, head, all, time.Now().Format("2006-01-02"), opt.AdoptName); err != nil {
				return nil, err
			}
			if res.Gate, err = gate.Dirs(ctx, z, opt.Registry, res.Head); err != nil {
				return nil, err
			}
			if head, err = ir.LoadDir(res.Head); err != nil {
				return nil, err
			}
		}
		if len(all) > 0 {
			for _, a := range answers {
				if a.Kind == "conflict" && a.Choice == "later" && a.By != Settler {
					res.Open++ // as the owner counts them: by question
				}
			}
		}
	}
	if res.Gate.Check != nil {
		var sets [][]string
		for _, cf := range res.Gate.Check.Conflicts {
			sets = append(sets, cf.Rules)
		}
		res.narrow = narrower(ctx, z, res.Head, head, sets)
		confirmBlocking(res, head)
		for _, cf := range res.Gate.Check.Conflicts {
			if ops, err := checker.CreatorsSettingAside(ctx, z, head, cf); err == nil && len(ops) > 0 {
				if res.creators == nil {
					res.creators = map[string][]string{}
				}
				res.creators[conflictKey(cf)] = ops
			}
		}
		res.reportedWrites = reportedWrites(head, res)
	}
	res.FlagOnly = flagOnly(head, res)
	res.Questions = questions(res, head, c, known, answers)
	// At an adoption the texts are reviewed too: a contradiction they
	// suggest and the rules do not show has its requirements read again.
	if opt.rounds < 5 {
		if n := crossCheck(ctx, opt, res, head, answers); n > 0 {
			opt.rounds++
			again, err := Run(ctx, opt)
			if err != nil {
				if again != nil {
					again.Usage.Add(res.Usage)
				}
				return again, err
			}
			again.Usage.Add(res.Usage)
			return again, nil
		}
	}
	// A rule in a conflict that does not say what its text says is set
	// aside before the owner is asked, and the check runs again without it.
	if opt.rounds < 3 {
		if n := audit(ctx, opt, res, head, c, answers); n > 0 {
			opt.rounds++
			again, err := Run(ctx, opt)
			if err != nil {
				if again != nil {
					again.Usage.Add(res.Usage)
				}
				return again, err
			}
			again.Usage.Add(res.Usage)
			return again, nil
		}
	}
	// What is about Stele's own rules and not about the specs, Stele
	// settles itself, and checks again: settling one clash can bring out
	// the next among the same rules.
	if opt.settles < 12 {
		if n := settle(opt, res, answers); n > 0 {
			if opt.Log != nil {
				fmt.Fprintf(opt.Log, "settle: %d question(s) about Stele's own rules answered; checking again\n", n)
			}
			opt.settles++
			again, err := Run(ctx, opt)
			if err != nil {
				if again != nil {
					again.Usage.Add(res.Usage)
				}
				return again, err
			}
			again.Usage.Add(res.Usage)
			return again, nil
		}
	}
	res.SettledOpen, res.SettledPrecedence, res.SettledUnchecked, _ = settled(answers)
	// A conflict that comes from glossary definitions left out of the
	// rules is read again with them before the owner is asked (ISSUES 62).
	if opt.rounds < 3 {
		if n := definitions(ctx, opt, res, head, c, checkFiles, answers); n > 0 {
			if err := c.save(opt.Out); err != nil {
				return nil, err
			}
			if opt.Log != nil {
				fmt.Fprintf(opt.Log, "definitions: %d world fact(s) from the glossary; checking again\n", n)
			}
			opt.rounds++
			again, err := Run(ctx, opt)
			if err != nil {
				if again != nil {
					again.Usage.Add(res.Usage)
				}
				return again, err
			}
			again.Usage.Add(res.Usage)
			return again, nil
		}
	}
	if !opt.NoCode {
		codeEvidence(ctx, opt, res)
	}
	res.Repo, res.Registry = opt.Repo, opt.Registry
	if res.Fingerprint, err = Fingerprint(res.Watched...); err != nil {
		return nil, err
	}
	if res.Fingerprint != read {
		// The result is for text no longer there, but what it spent is
		// spent: returned with the error, for the cost it reports.
		return res, ErrChangedDuringCheck
	}
	if res.Head != "" {
		if res.HeadFingerprint, err = Fingerprint(res.Head); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ErrChangedDuringCheck: a requirement or the registry was edited while the
// check ran, so its result describes text that is no longer there. Run
// returns its result with it only for the spending.
var ErrChangedDuringCheck = errors.New("the requirements or the registry changed while the check was running, so its result is for text that is no longer there. Next: run the check again")

// translateBatch is how many statements one translation request holds.
const translateBatch = 20

// translate formalizes the statements over the vocabulary and the terms
// the change proposed before, and records the result.
func (c *cache) translate(ctx context.Context, opt Options, todo []translate.Statement, vocabFiles []string, newIn string, known map[string][]string, res *Result) error {
	if len(opt.Clients) == 0 {
		return fmt.Errorf("%d statement(s) need translating and no model is configured", len(todo))
	}
	// formalize copies its vocabulary files next to the rules by base name,
	// and writes its own vocabulary.yaml there: stage them under unique names.
	stage := filepath.Join(opt.Out, "vocabulary")
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	var files []string
	for i, f := range vocabFiles {
		to := filepath.Join(stage, fmt.Sprintf("registry-%02d-%s", i+1, filepath.Base(f)))
		if err := copyFile(f, to); err != nil {
			return err
		}
		files = append(files, to)
	}
	seed, err := vocab.YAML(c.Proposal, newIn)
	if err != nil {
		return err
	}
	if len(seed) > 0 {
		f := filepath.Join(stage, "change-seed.yaml")
		if err := os.WriteFile(f, seed, 0o644); err != nil {
			return err
		}
		files = append(files, f)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", c.Digest, seed)
	for _, s := range todo {
		fmt.Fprintf(h, "%s\x00%s\x00", s.ID, key(s))
	}
	dir := filepath.Join(opt.Out, "runs", fmt.Sprintf("%x", h.Sum(nil)[:6]))
	// One fact, one name. Statements translated in one request see each
	// other and name a fact once; those of several requests, sent at once,
	// each name it their own way, and rules over different names never
	// meet. So when the statements span more than one request (an
	// adoption, a large change), the facts are proposed over all of them
	// first and every request reuses them: on two labelled held-out sets
	// that found 16 of 20 and 8 of 13 known contradictions against 4 and
	// 4, every finding but one a labelled one, for a fifth to two fifths
	// more spend. A run an earlier release started keeps its own way.
	factsFirst := len(todo) > translateBatch
	if factsFirst {
		if _, err := os.Stat(filepath.Join(dir, formalize.StateFile)); err == nil {
			factsFirst = false
		} else {
			fmt.Fprint(h, "facts-first\x00")
			dir = filepath.Join(opt.Out, "runs", fmt.Sprintf("%x", h.Sum(nil)[:6]))
		}
	}
	// A run already finished for these inputs is read back, not paid for again.
	finished := false
	if src, err := os.ReadFile(filepath.Join(dir, formalize.StateFile)); err == nil {
		finished = bytes.Contains(src, []byte(`"stage": "done"`))
		if bytes.Contains(src, []byte(`"compact_deferred"`)) {
			// A run an earlier release finished with failures recorded as
			// settled: start it again, so the failures are retried now.
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			finished = false
		}
	}
	var ks []vocab.Used
	for name, ids := range known {
		u := vocab.Used{Name: name}
		for k, id := range ids {
			if k < 2 {
				u.Examples = append(u.Examples, oneLine(res.Statements[id], 160))
			}
			u.Text += " " + res.Statements[id]
		}
		ks = append(ks, u)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].Name < ks[j].Name })
	log := opt.Log
	if log == nil {
		log = io.Discard
	}
	var others []translate.Statement
	for id, prose := range res.Statements {
		if !slices.ContainsFunc(todo, func(s translate.Statement) bool { return s.ID == id }) {
			others = append(others, translate.Statement{ID: id, Prose: prose, Source: res.Sources[id]})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].ID < others[j].ID })
	fr, err := formalize.Run(ctx, opt.Clients[0], todo, dir, formalize.Options{VocabFiles: files, Context: newIn, Compact: true, FactsFirst: factsFirst,
		Batch: translateBatch, Parallel: 4, MaxRepairs: 2, Known: ks, Others: others, Lessons: opt.lessons, Log: log, MaxCost: opt.MaxCost,
		RequestTimeout: opt.RequestTimeout})
	if err != nil {
		return fmt.Errorf("translate: %w", err)
	}
	if !finished {
		res.Usage.Add(fr.Usage)
	}
	c.Proposal = vocab.Merged(c.Proposal, fr.Proposal)
	byStmt := map[string][]translate.Candidate{}
	for _, cd := range fr.Cands {
		byStmt[cd.StatementID] = append(byStmt[cd.StatementID], cd)
	}
	findings := map[string][]string{}
	uncovered := map[string][]string{}
	coverFailed := map[string]bool{}
	retry := false
	for _, e := range fr.Ends {
		coverFailed[e.ID] = e.CoverFailed != ""
		uncovered[e.ID] = e.Uncovered
		for _, u := range e.Uncovered {
			findings[e.ID] = append(findings[e.ID], fmt.Sprintf("no rule checks %q", u))
		}
		for _, b := range e.Broader {
			findings[e.ID] = append(findings[e.ID], "broader than the text: "+b)
		}
		if e.Blocks != "" {
			findings[e.ID] = append(findings[e.ID], "blocks a step: "+e.Blocks)
		}
		if e.CoverFailed != "" {
			findings[e.ID] = append(findings[e.ID], "not judged: Stele's check that its reading says what the text says did not run ("+firstLine(e.CoverFailed)+"); run the check again")
		}
	}
	for _, s := range todo {
		why, failed := fr.Deferred[s.ID]
		failed = failed && len(byStmt[s.ID]) == 0
		if !failed && len(uncovered[s.ID]) > 0 && !slices.ContainsFunc(byStmt[s.ID], func(c translate.Candidate) bool { return c.Outcome == "rule" }) {
			// Read as having nothing to check, yet the coverage judge finds
			// clauses a rule can check: the model skipped the translation,
			// and the next check retries it. The same text read so again is
			// what the model makes of it: it is unchecked, each clause a
			// finding the owner sees. Failed every time, a plot listing's
			// display requirement held its change back, and the agent
			// offered to reword the spec to suit Stele (a developer trial,
			// 2026-11-05).
			if c.NoRules[s.ID] != key(s) {
				if c.NoRules == nil {
					c.NoRules = map[string]string{}
				}
				c.NoRules[s.ID] = key(s)
				why, failed = NoRules+", though the text has clauses a rule can check: "+strings.Join(uncovered[s.ID], "; "), true
			}
		}
		if failed {
			if res.Failed == nil {
				res.Failed = map[string]string{}
			}
			res.Failed[s.ID] = strings.TrimSpace(why)
			continue // not cached: the next check translates it again
		}
		k := key(s)
		if coverFailed[s.ID] {
			// Used by this check, never matched by the next, and the run is
			// started afresh: the next check judges it again (a code
			// review, 2026-11-03).
			k, retry = "retry:"+k, true
		}
		c.Entries[s.ID] = entry{Key: k, Cands: byStmt[s.ID], Findings: findings[s.ID]}
	}
	if len(fr.Deferred) > 0 || retry {
		// The run's state records the failures as settled; a rerun with the
		// same statements must call the model again, not read them back.
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	prefix := fmt.Sprintf("world-%s-", safe(opt.Change))
	n := lastNumber(c.Links, prefix)
	seen := map[string]bool{}
	for i := range c.Links { // numbered twice before lastNumber
		if seen[c.Links[i].RuleID] {
			n++
			c.Links[i].RuleID = fmt.Sprintf("%s%d", prefix, n)
		}
		seen[c.Links[i].RuleID] = true
	}
	for _, l := range fr.LinkCands {
		if l.Error != "" {
			continue
		}
		n++
		l.RuleID = fmt.Sprintf("%s%d", prefix, n)
		l.StatementID = ""
		c.addLinks([]translate.Candidate{l})
	}
	for _, f := range fr.Flags {
		if !slices.Contains(c.Flags, f) {
			c.Flags = append(c.Flags, f)
		}
	}
	c.Merges = append(c.Merges, fr.Merges...)
	return nil
}

// lastNumber is the highest n of the links with ID prefix+n. New world facts
// are numbered after it: by their count, a fact added after one was
// rejected took the ID of the last, and the change's registry no longer
// loaded ("rule world-main-3 declared twice").
func lastNumber(links []translate.Candidate, prefix string) int {
	n := 0
	for _, l := range links {
		if k, err := strconv.Atoi(strings.TrimPrefix(l.RuleID, prefix)); err == nil && strings.HasPrefix(l.RuleID, prefix) {
			n = max(n, k)
		}
	}
	return n
}

// contextOf is the context most of the registry's entities belong to:
// where the terms a change adds go.
// RenameGuess is a removed requirement and an added one that read alike.
type RenameGuess struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Similarity float64 `json:"similarity"` // share of words in common, 0 to 1
}

// renameGuessThreshold is the share of words a removed and an added
// requirement must share to be put to the owner as one requirement renamed.
const renameGuessThreshold = 0.5

// guessRenames pairs each removed statement with the added statement of the
// same capability it most resembles, when they resemble each other enough
// and the owner has not already said they differ.
func guessRenames(rep *adopt.SyncReport, prose map[string]string, as []Answer) []RenameGuess {
	var out []RenameGuess
	taken := map[string]bool{}
	for _, from := range rep.Removed {
		cap, _, _ := strings.Cut(from, "/")
		best, score := "", 0.0
		for _, to := range rep.Added {
			if c, _, _ := strings.Cut(to, "/"); c != cap || taken[to] {
				continue
			}
			if s := adopt.Similarity(prose[from], prose[to]); s > score {
				best, score = to, s
			}
		}
		if best == "" || score < renameGuessThreshold {
			continue
		}
		if slices.ContainsFunc(as, func(a Answer) bool {
			return a.Kind == "renamed" && a.Term == from && a.Existing == best && a.Choice == "different"
		}) {
			continue
		}
		taken[best] = true
		out = append(out, RenameGuess{From: from, To: best, Similarity: score})
	}
	return out
}

// keepRuleIDs gives the rules translated from a changed statement the IDs of
// the live rules it had, in order, so they take a new version instead of
// retiring the old ones and arriving as strangers. This matters when the
// statement was renamed: its rules were named after the old title. Further
// rules continue the old numbering past any ID the registry already has.
func keepRuleIDs(cands []translate.Candidate, rulesOf map[string][]string, base *ir.Registry) {
	for stmt, pins := range rulesOf {
		var old []string
		for _, p := range pins {
			id, _, _ := strings.Cut(p, "@")
			old = append(old, id)
		}
		if len(old) == 0 {
			continue
		}
		k := 0
		next := len(old) + 1
		for i := range cands {
			c := &cands[i]
			if c.StatementID != stmt || c.Outcome != "rule" {
				continue
			}
			if k < len(old) {
				c.RuleID = old[k]
			} else {
				for ; base.Rule(fmt.Sprintf("%s.%d", old[0], next)) != nil; next++ {
				}
				c.RuleID = fmt.Sprintf("%s.%d", old[0], next)
				next++
			}
			k++
		}
	}
}

func contextOf(reg *ir.Registry) string {
	n := map[string]int{}
	best := "main"
	for _, e := range reg.Entities {
		if e.Context == "" || e.Context == "platform" {
			continue
		}
		n[e.Context]++
		if n[e.Context] > n[best] || n[e.Context] == n[best] && e.Context < best {
			best = e.Context
		}
	}
	return best
}

// knownUses maps each attribute the registry's statement rules read to
// those statements, so a new term can be compared with the facts the
// spec already names, in the spec's own words.
func knownUses(reg *ir.Registry, prose map[string]string) map[string][]string {
	out := map[string][]string{}
	for _, x := range reg.Rules {
		if x.StatementID == "" || !x.Live() || prose[x.StatementID] == "" {
			continue
		}
		subject, err := reg.SubjectOf(x)
		if err != nil {
			continue
		}
		var reads []*ir.Attr
		if x.When != nil {
			reads = append(reads, reg.Reads(x.When, subject)...)
		}
		if x.Assert != nil {
			reads = append(reads, reg.Reads(x.Assert, subject)...)
		}
		for _, a := range reads {
			if a.Entity == nil || a.Entity.Name == "Request" || a.Entity.Name == "Actor" {
				continue
			}
			q := a.QName()
			if !slices.Contains(out[q], x.StatementID) {
				out[q] = append(out[q], x.StatementID)
			}
		}
	}
	for q := range out {
		sort.Strings(out[q])
	}
	return out
}

// Fingerprint identifies the files under the roots (the change's specs and
// the registry), so a recorded check can be told apart from a stale one.
func Fingerprint(roots ...string) (string, error) {
	h := sha256.New()
	for _, root := range roots {
		// Shale appends every command an agent runs to the open session's
		// file, so its folder changes between the check and the approval.
		// What the check read is the requests: fingerprint those.
		if filepath.Base(root) == ".shale" {
			stmts, err := importer.Shale(filepath.Dir(root))
			if err != nil {
				return "", err
			}
			for _, s := range stmts {
				fmt.Fprintf(h, "%s\x00%d\x00%s", s.ID, len(s.Prose), s.Prose)
			}
			continue
		}
		// A folder that is not there yet holds nothing: a new project's
		// requirements folder before its first requirement.
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			fmt.Fprintf(h, "%s\x00%d\x00", rel, len(src))
			h.Write(src)
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:12]), nil
}

// writeLinks writes world facts the change's translation proposed as rules,
// or removes the file when there are none.
func writeLinks(path string, links []translate.Candidate) error {
	var b strings.Builder
	for _, l := range links {
		if l.Error != "" {
			continue
		}
		fmt.Fprintf(&b, "---\nid: %s\nstatus: active\nkind: invariant\n", l.RuleID)
		if l.On != "" {
			fmt.Fprintf(&b, "on: %s\n", l.On)
		}
		if l.Assume {
			b.WriteString("assume: true\n")
		}
		if l.When != "" {
			fmt.Fprintf(&b, "when: %q\n", l.When)
		}
		fmt.Fprintf(&b, "assert: %q\nsource:\n  inferredBy: stele change\n  prose: %q\n", l.Assert, "World fact: "+l.Rationale)
	}
	if b.Len() == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(path, []byte("# World facts this change's translation assumed: how two things relate by their nature.\n"+b.String()), 0o644)
}

func copyFile(from, to string) error {
	src, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, src, 0o644)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	// Cut between words, so "up to 10 books" never reads "up to 1...".
	cut := s[:n]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, " ,;:") + "..."
}

// VocabFiles are the registry's YAML files that hold only vocabulary,
// contexts and world facts: not the rules translated from statements, nor
// the decisions and open conflicts that name them.
func VocabFiles(dir string, reg *ir.Registry) ([]string, error) {
	skip := map[string]bool{}
	add := func(f string) {
		if i := strings.LastIndexByte(f, ':'); i > 0 {
			f = f[:i]
		}
		skip[filepath.Clean(f)] = true
	}
	for _, x := range reg.Rules {
		if x.StatementID != "" {
			add(x.File)
		}
	}
	for _, d := range reg.Decisions {
		add(d.File)
	}
	for _, o := range reg.Open {
		add(o.File)
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")) && !skip[filepath.Clean(p)] {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

func readDocs(path string) ([]*yaml.Node, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var out []*yaml.Node
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode && scalar(doc.Content[0], "id") != "" {
			out = append(out, doc.Content[0])
		}
	}
	return out, nil
}

func writeDocs(path string, docs []*yaml.Node) error {
	var b bytes.Buffer
	b.WriteString("# Rules translated for an OpenSpec change by `stele change`; approved by answering the check's approval question (`stele change answer`).\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(d); err != nil {
			return err
		}
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

func scalar(n *yaml.Node, key string) string {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1].Value
		}
	}
	return ""
}

func setScalar(n *yaml.Node, key, value string) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			n.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Value: value}
			return
		}
	}
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, src, 0o644)
	})
}

// RegistryPrefix names the registry files a change writes when it is
// approved: <prefix>.yaml, <prefix>.vocabulary.yaml and the like.
func RegistryPrefix(change string) string { return "change-" + safe(change) }

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ' ' {
			return '-'
		}
		return r
	}, s)
}

// writeCreators records which operations create which entities, as the
// answers say, or removes the file when none do.
func writeCreators(path string, by map[string][]string, context string) error {
	if len(by) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	var b strings.Builder
	b.WriteString("# Steps that create their entity, as answered during `stele change`.\n")
	ents := make([]string, 0, len(by))
	for e := range by {
		ents = append(ents, e)
	}
	sort.Strings(ents)
	for _, e := range ents {
		ops := append([]string{}, by[e]...)
		sort.Strings(ops)
		fmt.Fprintf(&b, "---\nextend: %s\ncontext: %s\ncreatedBy: [%s]\n", e, context, strings.Join(ops, ", "))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeFile(path, src string) error { return os.WriteFile(path, []byte(src), 0o644) }

// lessonsFrom turns the owner's answers into lessons for the registry.
// approvedStatements are the statements the registry's rules come from.
func approvedStatements(reg *ir.Registry) []string {
	var ids []string
	for _, r := range reg.Rules {
		ids = append(ids, r.StatementID)
	}
	return ids
}

// Answers the agent gave itself correct Stele's own reading and stay notes
// for this change; they teach nothing about the project.
func lessonsFrom(as []Answer, change string) []ir.Lesson {
	var out []ir.Lesson
	at := time.Now().UTC().Format("2006-01-02")
	for _, a := range as {
		if a.By == "agent" {
			continue
		}
		from := "stele change " + change + ": " + a.Question
		switch {
		case a.Kind == "same_term" && a.Choice == "same" && a.Term != "" && a.Existing != "":
			out = append(out, ir.Lesson{Kind: ir.LessonTerm, Says: fmt.Sprintf("%s and %s name the same thing; use %s and do not propose %s.", a.Term, a.Existing, a.Existing, a.Term),
				Terms: []string{a.Term, a.Existing}, From: from, By: a.By, At: at})
		case !a.Quoted:
			// Only the owner's own words teach: see Answer.Quoted.
		case a.Kind == "conflict" && a.Choice == "impossible" && strings.TrimSpace(a.Note) != "":
			says := strings.TrimSpace(a.Note)
			if a.Said != "" {
				says = a.Said // the owner's words, not the solver's situation
			}
			out = append(out, ir.Lesson{Kind: ir.LessonFact, Says: "This situation cannot happen in this project: " + says,
				From: from, By: a.By, At: at})
		case (a.Kind == "reread" || ((a.Kind == "conflict" || a.Kind == "self_conflict") && a.Choice == "misread") || a.Kind == "free_setting" && a.Choice == "fixed") && strings.TrimSpace(a.Note) != "" && len(a.Statements) > 0:
			out = append(out, ir.Lesson{Kind: ir.LessonReading, Says: strings.TrimSpace(a.Note), Statements: a.Statements, From: from, By: a.By, At: at})
		}
	}
	return out
}

// confirmBlocking keeps a "blocks a step" finding only while the head
// registry still shows it. Translation checks a statement's rules before
// the change's other rules and the writes Stele infers for them are in
// place: a rule that set a fact its step did not yet write blocked the step
// there and not in the head (a first-use test, 2026-11-01), and a finding
// no check can show is one the owner cannot act on.
func confirmBlocking(res *Result, head *ir.Registry) {
	still := map[string]bool{}
	for _, cf := range res.Gate.Check.Conflicts {
		if len(cf.Rules) == 1 && cf.Unit != "" && cf.Unit != "state" {
			if x := head.Rule(cf.Rules[0]); x != nil {
				still[x.StatementID] = true
			}
		}
	}
	for id, fs := range res.Findings {
		var kept []string
		for _, f := range fs {
			if strings.HasPrefix(f, "blocks a step: ") && !still[id] {
				continue
			}
			kept = append(kept, f)
		}
		if len(kept) == 0 {
			delete(res.Findings, id)
		} else {
			res.Findings[id] = kept
		}
	}
}

// approvedAlready reports whether reg holds approved, live rules for every
// statement in texts, translated from that very text.
func approvedAlready(reg *ir.Registry, texts map[string]string) bool {
	if len(texts) == 0 {
		return false
	}
	for id, text := range texts {
		found := false
		for _, x := range reg.Rules {
			if x.StatementID != id || !x.Live() {
				continue
			}
			if !x.Approved() || x.TextHash != translate.TextHash(text) {
				return false
			}
			found = true
		}
		if !found {
			return false
		}
	}
	return true
}

// unusable records, as a finding of its requirement, a reading Stele could
// not use. A requirement checked in part says so: its other rules made it
// look checked, and the clauses this rule read went unchecked without a
// word (a developer trial, 2026-11-03).
func unusable(res *Result, c translate.Candidate) {
	if res.Findings == nil {
		res.Findings = map[string][]string{}
	}
	f := "not checked: Stele could not use its reading of part of this requirement (" + englishCand(c) + "): " + firstLine(c.Error)
	if !slices.Contains(res.Findings[c.StatementID], f) {
		res.Findings[c.StatementID] = append(res.Findings[c.StatementID], f)
	}
}

// settleInvalid deals with the requirements none of whose readings
// compile. Such a requirement had no rule and no record, so its change read
// as clean with nothing to approve and the requirement stayed new for ever
// (a trial, 2026-11-09). The first time it fails, and the next check reads
// it again (again is true); read so twice from the same text, it goes to
// the approval as a requirement Stele records and does not check.
func (c *cache) settleInvalid(all []translate.Candidate, pending []translate.Statement, res *Result) (out []translate.Candidate, again bool) {
	for _, s := range pending {
		first, usable := "", false
		for _, cd := range all {
			if cd.StatementID != s.ID {
				continue
			}
			if cd.Error == "" {
				usable = true
			} else if first == "" {
				first = cd.Error
			}
		}
		if usable || first == "" {
			continue
		}
		if c.Invalid[s.ID] == key(s) {
			all = append(all, translate.Candidate{StatementID: s.ID, RuleID: s.ID, Outcome: ir.Informal, Kind: "none",
				Rationale: "Stele's reading of this requirement does not compile, so no rule checks it"})
			continue
		}
		if c.Invalid == nil {
			c.Invalid = map[string]string{}
		}
		c.Invalid[s.ID] = key(s)
		delete(c.Entries, s.ID)
		all = slices.DeleteFunc(all, func(cd translate.Candidate) bool { return cd.StatementID == s.ID })
		if res.Failed == nil {
			res.Failed = map[string]string{}
		}
		res.Failed[s.ID] = "Stele's reading did not compile: " + oneLine(first, 200)
		again = true
	}
	return all, again
}

// englishCand is a short plain reading of a candidate rule.
func englishCand(c translate.Candidate) string {
	var parts []string
	if c.On != "" {
		parts = append(parts, "on "+c.On)
	} else if c.Operation != "" {
		parts = append(parts, c.Kind+" "+c.Effect+" "+c.Operation)
	}
	if c.When != "" {
		parts = append(parts, "when "+c.When)
	}
	if c.Assert != "" {
		parts = append(parts, "then "+c.Assert)
	}
	return strings.Join(parts, " ")
}

// firstLine is the first line of s.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// conflictKey names a conflict: its step and rules.
func conflictKey(cf checker.Conflict) string {
	return cf.Unit + ":" + strings.Join(cf.Rules, "+")
}

// Adoption is the change that reads a project's existing specs.
const Adoption = "adopt"

// leftOpen lists the conflicts of an adoption the owner answered "later":
// every requirement of the conflict is one the answer was given for, as it
// is now written. A first adoption of 179 requirements asked its owner 12
// conflicts, then 8 and 13 over two more rounds (2026-11-10); the registry
// and the gate have held open conflicts since D-044, and the questions
// never offered them. A new user approves what they can decide today, and
// the rest stays listed until a change settles it; the gate lets the count
// only fall.
func leftOpen(res *Result, head *ir.Registry, answers []Answer) []adopt.LeftOpen {
	var out []adopt.LeftOpen
	later := func(rules []string) string {
		var stmts []string
		for _, r := range rules {
			if x := head.Rule(r); x != nil && x.StatementID != "" && !slices.Contains(stmts, x.StatementID) {
				stmts = append(stmts, x.StatementID)
			}
		}
		if len(stmts) == 0 {
			return ""
		}
		for _, a := range answers {
			if a.Kind != "conflict" || a.Choice != "later" {
				continue
			}
			if !slices.ContainsFunc(stmts, func(id string) bool { return a.Statements[id] != translate.TextHash(res.Statements[id]) }) {
				return a.By
			}
		}
		return ""
	}
	for _, cf := range res.Gate.Check.Conflicts {
		if by := later(cf.Rules); by != "" {
			out = append(out, adopt.LeftOpen{Rules: cf.Rules, Situation: plainSituation(checker.Situation(head, cf.Counterexample)), By: by})
		}
	}
	// The same requirements' clash as a step that can never run: it shows
	// once their conflict is known, and was the one thing a first adoption
	// had left to ask after every conflict was decided (2026-11-10).
	for _, d := range res.Gate.Check.Dead {
		if by := later(d.Rules); by != "" && !d.NeverAllowed {
			out = append(out, adopt.LeftOpen{Rules: d.Rules, Response: d.Response, Situation: strings.Trim(Words(d.Unit, ""), `"`) + " can never run", By: by})
		}
	}
	return out
}

// leftUnchecked reports whether the owner answered that a requirement whose
// readings contradict themselves is to be recorded without rules. Asked
// only "what did Stele misread?" or "reword it", the owner of a 76-
// requirement adoption owed three explanations of Stele's own misreadings
// before anything could be approved (2026-11-10).
func leftUnchecked(as []Answer, id, hash string) bool {
	for _, a := range as {
		if a.Kind == "self_conflict" && a.Choice == "unchecked" && a.Statements[id] == hash {
			return true
		}
	}
	return false
}

// dirHasReadings reports whether a check already keeps readings: a review
// made after them steers by a second reading, not the first.
func dirHasReadings(out string) bool {
	src, err := os.ReadFile(filepath.Join(out, cacheFile))
	if err != nil {
		return false
	}
	var c struct {
		Entries map[string]json.RawMessage `json:"entries"`
	}
	return json.Unmarshal(src, &c) == nil && len(c.Entries) > 0
}
