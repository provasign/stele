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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/provasign/stele/internal/adopt"
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
	Adopt    []translate.Statement
	SpecsDir string
	Clients  []llm.Client
	Z3       *solver.Z3
	Log      io.Writer
	// NoCode skips asking what the code does for each conflict (D-062);
	// Graph also finds definitions in Grove's code graph (no gain measured
	// on 2026-10-29, so off by default).
	NoCode, Graph bool
	// MaxCost caps the translation's model calls, in dollars as the backend
	// reports them (formalize.Options.MaxCost); 0 is unlimited.
	MaxCost float64

	rounds  int          // times the glossary's definitions added facts this run
	lessons []*ir.Lesson // the registry's lessons, for the coverage judge
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
	Usage         llm.Usage     `json:"usage"`
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
	Approved bool     `json:"approved,omitempty"`
	Watched  []string `json:"watched"` // the roots the fingerprint covers
	Repo     string   `json:"repo"`
	Registry string   `json:"registry"`
	// narrow maps a pair of conflicting statements to the one whose
	// condition lies inside the other's.
	narrow map[string]string
}

// Run checks the change.
func Run(ctx context.Context, opt Options) (*Result, error) {
	res := &Result{Change: opt.Change, Format: opt.Format, Rules: map[string][]string{}, Unchecked: map[string]string{},
		Statements: map[string]string{}, Sources: map[string]string{}}
	var truth, after []translate.Statement
	var err error
	switch {
	case opt.Adopt != nil:
		after = opt.Adopt
		for _, s := range after {
			res.Added = append(res.Added, s.ID)
		}
		res.Watched = []string{opt.SpecsDir}
	case opt.Base == "":
		if truth, after, err = openSpecChange(opt, res); err != nil {
			return nil, err
		}
		res.Watched = []string{filepath.Join(opt.Repo, "openspec", "changes", opt.Change)}
	default:
		if truth, after, err = branchChange(opt, res); err != nil {
			return nil, err
		}
		res.Watched = []string{filepath.Join(opt.Repo, specRoot(opt.Format))}
	}
	res.Watched = append(res.Watched, opt.Registry)
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

	// Translate what the change adds or edits, over the registry's
	// vocabulary and the terms the change proposes.
	for i := range pending {
		pending[i].Notes = append(pending[i].Notes, notes(answers, pending[i])...)
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
			c.Links = slices.DeleteFunc(c.Links, func(l translate.Candidate) bool { return l.RuleID == a.Link })
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
			if e, ok := c.Entries[s.ID]; ok && e.Key == key(s) {
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
			all = append(all, c.Entries[s.ID].Cands...)
			if f := c.Entries[s.ID].Findings; len(f) > 0 {
				if res.Findings == nil {
					res.Findings = map[string][]string{}
				}
				res.Findings[s.ID] = f
			}
		}
		keepRuleIDs(all, rep.RulesOf, base)
		textPrecedence(all, res.Statements)
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
		translate.Revalidate(files, all)
		if reg, err := ir.LoadFiles(files...); err == nil {
			res.InferredWrites = inferWrites(reg, p, all)
		}
		linkCands = append(linkCands, c.Links...)
		translate.Revalidate(files, linkCands)
		checkFiles = files
		if err := translate.WriteRegistry(dir, &translate.Report{Backend: "stele change", Statements: len(pending), Candidates: all, Inputs: pending}, pending, nil); err != nil {
			return nil, err
		}
		reasons := map[string]string{}
		for _, c := range all {
			switch {
			case c.Error != "":
				reasons[c.StatementID] = "invalid: " + c.Error
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
	prefix := "change-" + safe(opt.Change)
	if opt.Adopt != nil {
		prefix = "adopted"
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
	if err := writeWrites(filepath.Join(res.Head, prefix+".writes.yaml"), res.InferredWrites); err != nil {
		return nil, err
	}
	if err := writeCreators(filepath.Join(res.Head, prefix+".created.yaml"), creators(answers), newIn); err != nil {
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
	if res.Gate.Check != nil {
		var sets [][]string
		for _, cf := range res.Gate.Check.Conflicts {
			sets = append(sets, cf.Rules)
		}
		res.narrow = narrower(ctx, z, res.Head, head, sets)
		confirmBlocking(res, head)
	}
	res.Questions = questions(res, head, c, known, answers)
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
				return nil, err
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
	return res, nil
}

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
	// A run already finished for these inputs is read back, not paid for again.
	finished := false
	if src, err := os.ReadFile(filepath.Join(dir, formalize.StateFile)); err == nil {
		finished = bytes.Contains(src, []byte(`"stage": "done"`))
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
	fr, err := formalize.Run(ctx, opt.Clients[0], todo, dir, formalize.Options{VocabFiles: files, Context: newIn, Compact: true,
		Batch: 20, Parallel: 4, MaxRepairs: 2, Known: ks, Others: others, Lessons: opt.lessons, Log: log, MaxCost: opt.MaxCost})
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
	for _, e := range fr.Ends {
		for _, u := range e.Uncovered {
			findings[e.ID] = append(findings[e.ID], fmt.Sprintf("no rule checks %q", u))
		}
		for _, b := range e.Broader {
			findings[e.ID] = append(findings[e.ID], "broader than the text: "+b)
		}
		if e.Blocks != "" {
			findings[e.ID] = append(findings[e.ID], "blocks a step: "+e.Blocks)
		}
	}
	for _, s := range todo {
		c.Entries[s.ID] = entry{Key: key(s), Cands: byStmt[s.ID], Findings: findings[s.ID]}
	}
	n := len(c.Links)
	for _, l := range fr.LinkCands {
		if l.Error != "" {
			continue
		}
		n++
		l.RuleID = fmt.Sprintf("world-%s-%d", safe(opt.Change), n)
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
	b.WriteString("# Rules translated for an OpenSpec change by `stele change`; see `stele approve`.\n")
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
		case a.Kind == "conflict" && a.Choice == "impossible" && strings.TrimSpace(a.Note) != "":
			out = append(out, ir.Lesson{Kind: ir.LessonFact, Says: "This situation cannot happen in this project: " + strings.TrimSpace(a.Note),
				From: from, By: a.By, At: at})
		case (a.Kind == "reread" || ((a.Kind == "conflict" || a.Kind == "self_conflict") && a.Choice == "misread")) && strings.TrimSpace(a.Note) != "" && len(a.Statements) > 0:
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
