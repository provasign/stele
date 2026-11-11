package change

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/review"
	"github.com/provasign/stele/internal/translate"
)

// Whether the rules show a contradiction depended on one reading of each
// requirement: two runs over one labelled set of 45 requirements found 8
// and 5 of its 13 known contradictions (2026-11-10). A review of the texts
// by the same model, with no rules, found 13 and 11 of them for two cents.
// At an adoption the texts are therefore reviewed once. A contradiction the
// review names and the rules do not show has its requirements read once
// more, told what the review saw; one the rules still do not show is listed
// for the owner as possible, so that finding it no longer hangs on the
// first reading. The solver stays the proof: only a contradiction of the
// rules becomes a question.

// crossFile holds an adoption's review of its texts and which of its
// findings already steered a reading.
const crossFile = "review.json"

// Possible is a contradiction a review of the texts found that the rules
// do not show.
type Possible struct {
	Statements []string `json:"statements"`
	Titles     []string `json:"titles,omitempty"`
	Situation  string   `json:"situation"`
	// Why says what kept the rules from showing it.
	Why string `json:"why"`
}

type crossState struct {
	// Key identifies the texts reviewed.
	Key      string           `json:"key"`
	Findings []review.Finding `json:"findings"`
	// Steered maps a finding (its statements, joined) to true once its
	// requirements were read again with it.
	Steered map[string]bool `json:"steered,omitempty"`
	// Notes are the steering notes by statement, while its text is the
	// one hashed.
	Notes map[string]crossNote `json:"notes,omitempty"`
}

type crossNote struct {
	Hash string `json:"hash"`
	Note string `json:"note"`
}

func loadCross(dir string) crossState {
	var st crossState
	if src, err := os.ReadFile(filepath.Join(dir, crossFile)); err == nil {
		_ = json.Unmarshal(src, &st)
	}
	if st.Steered == nil {
		st.Steered = map[string]bool{}
	}
	if st.Notes == nil {
		st.Notes = map[string]crossNote{}
	}
	return st
}

func (st crossState) save(dir string) error {
	src, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err // before the first reading the folder may not be there yet
	}
	return os.WriteFile(filepath.Join(dir, crossFile), append(src, '\n'), 0o644)
}

// crossNotes are the steering notes for a statement as it is now written.
func crossNotes(dir string, s translate.Statement) []string {
	st := loadCross(dir)
	if n, ok := st.Notes[s.ID]; ok && n.Hash == translate.TextHash(s.Prose) {
		return []string{n.Note}
	}
	return nil
}

// crossCheck reviews an adoption's texts once and compares what the review
// found with what the rules show. It returns how many requirements it sent
// to be read again; when none, res.Possible lists what stays unshown.
func crossCheck(ctx context.Context, opt Options, res *Result, head *ir.Registry, as []Answer) int {
	if opt.Adopt == nil || opt.NoReview || len(opt.Clients) == 0 || len(opt.Adopt) == 0 || head == nil || res.Gate == nil || res.Gate.Check == nil {
		return 0
	}
	st, adopting, earlier, ok := reviewTexts(ctx, opt, res, head)
	if !ok {
		return 0
	}
	// What the rules show, or the owner settled: pairs of statements.
	shown := map[string]bool{}
	pair := func(a, b string) string {
		if a > b {
			a, b = b, a
		}
		return a + "\x00" + b
	}
	mark := func(rules []string) {
		var ids []string
		for _, r := range rules {
			id, _, _ := strings.Cut(r, "@")
			if x := head.Rule(id); x != nil && x.StatementID != "" && !slices.Contains(ids, x.StatementID) {
				ids = append(ids, x.StatementID)
			}
		}
		for i := range ids {
			for j := i + 1; j < len(ids); j++ {
				shown[pair(ids[i], ids[j])] = true
			}
		}
	}
	for _, cf := range res.Gate.Check.Conflicts {
		mark(cf.Rules)
	}
	for _, d := range res.Gate.Check.Dead {
		mark(d.Rules)
	}
	for _, o := range res.Gate.Open {
		mark(o.Rules)
	}
	for _, x := range head.Rules {
		if x.Live() && x.StatementID != "" {
			for _, o := range x.Overrides {
				shown[pair(x.StatementID, o)] = true // settled by precedence
			}
		}
	}
	for _, a := range as {
		if a.Kind == "conflict" && a.By != "agent" {
			ids := slices.Sorted(func(yield func(string) bool) {
				for id := range a.Statements {
					if !yield(id) {
						return
					}
				}
			})
			ids = append(ids, a.Over...)
			for i := range ids {
				for j := i + 1; j < len(ids); j++ {
					shown[pair(ids[i], ids[j])] = true // the owner answered about them
				}
			}
		}
	}
	text := map[string]string{}
	for _, s := range append(slices.Clone(opt.Adopt), earlier...) {
		text[s.ID] = s.Prose
	}
	title := func(id string) string {
		t, _, _ := strings.Cut(text[id], "\nGlossary: ")
		return id + ": " + oneLine(strings.Join(strings.Fields(t), " "), 160)
	}
	steer := 0
	var possible []Possible
	for _, f := range st.Findings {
		ids := slices.DeleteFunc(slices.Clone(f.StatementIDs), func(id string) bool { return text[id] == "" })
		sort.Strings(ids)
		ids = slices.Compact(ids)
		if len(ids) < 2 || !slices.ContainsFunc(ids, func(id string) bool { return adopting[id] }) {
			continue // among requirements approved earlier: not this adoption's
		}
		met := 0
		for i := range ids {
			for j := i + 1; j < len(ids); j++ {
				if shown[pair(ids[i], ids[j])] {
					met++
				}
			}
		}
		if met > 0 {
			continue // the rules show it, at least in part: it is a question already, or settled
		}
		fk := strings.Join(ids, "\x00")
		var unruled []string
		for _, id := range ids {
			if len(res.Rules[id]) == 0 {
				unruled = append(unruled, id)
			}
		}
		if !st.Steered[fk] {
			st.Steered[fk] = true
			for _, id := range ids {
				if !adopting[id] {
					continue // approved: read as it was
				}
				var others []string
				for _, o := range ids {
					if o != id {
						others = append(others, title(o))
					}
				}
				note := steerNote(others, f.Situation, true)
				if old, ok := st.Notes[id]; ok && old.Hash == translate.TextHash(text[id]) {
					note = old.Note + " " + strings.TrimPrefix(note, "review: ")
				}
				st.Notes[id] = crossNote{Hash: translate.TextHash(text[id]), Note: note}
				steer++
			}
			continue
		}
		why := "the rules read from these requirements hold together"
		if len(unruled) > 0 {
			why = "Stele has no rule for " + quoteAll(unruled) + ", so nothing checks it"
		}
		p := Possible{Statements: ids, Situation: strings.TrimSpace(f.Situation), Why: why}
		possible = append(possible, p)
	}
	_ = st.save(opt.Out)
	if steer > 0 {
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, "review: reading %d requirement(s) again, in contradictions the texts suggest and the rules do not show\n", steer)
		}
		return steer
	}
	res.Possible = mergePossible(possible)
	return 0
}

// mergePossible joins possible contradictions that share a requirement into
// one, the way conflicts are grouped: one review named "deleting an account
// deletes its invoices" three times, once for each pair of the three
// requirements involved (2026-11-10).
func mergePossible(ps []Possible) []Possible {
	group := make([]int, len(ps))
	for i := range group {
		group[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if group[i] != i {
			group[i] = find(group[i])
		}
		return group[i]
	}
	for i := range ps {
		for j := i + 1; j < len(ps); j++ {
			if slices.ContainsFunc(ps[i].Statements, func(id string) bool { return slices.Contains(ps[j].Statements, id) }) {
				group[find(j)] = find(i)
			}
		}
	}
	var out []Possible
	at := map[int]int{}
	for i, p := range ps {
		r := find(i)
		k, ok := at[r]
		if !ok {
			at[r] = len(out)
			out = append(out, Possible{Statements: slices.Clone(p.Statements), Situation: p.Situation, Why: p.Why})
			continue
		}
		for _, id := range p.Statements {
			if !slices.Contains(out[k].Statements, id) {
				out[k].Statements = append(out[k].Statements, id)
			}
		}
		if out[k].Why != p.Why && !strings.Contains(out[k].Why, p.Why) {
			out[k].Why += "; " + p.Why
		}
	}
	for i := range out {
		sort.Strings(out[i].Statements)
	}
	return out
}

// reviewTexts returns an adoption's review of its texts, running it when
// none is kept for these texts: two reviews at once, their findings joined
// (one review of 45 requirements named 16 contradictions and another 11,
// and each costs cents). A part of the specs is reviewed with the
// requirements approved before it, in reg, or a contradiction between the
// two would have no backing. Run before the first reading, each finding
// becomes a note for its requirements, so that reading is steered at once:
// read afterwards, the same hints cost two more rounds of reading and
// checking, two minutes of eleven (a live session, 2026-11-10).
func reviewTexts(ctx context.Context, opt Options, res *Result, reg *ir.Registry) (st crossState, adopting map[string]bool, earlier []translate.Statement, ok bool) {
	adopting = map[string]bool{}
	for _, s := range opt.Adopt {
		adopting[s.ID] = true
	}
	if reg != nil {
		for _, x := range reg.Rules {
			if x.Live() && x.StatementID != "" && !adopting[x.StatementID] && x.Prose != "" &&
				!slices.ContainsFunc(earlier, func(s translate.Statement) bool { return s.ID == x.StatementID }) {
				earlier = append(earlier, translate.Statement{ID: x.StatementID, Prose: x.Prose, Source: x.Source})
			}
		}
	}
	sort.Slice(earlier, func(i, j int) bool { return earlier[i].ID < earlier[j].ID })
	h := sha256.New()
	for _, s := range append(slices.Clone(opt.Adopt), earlier...) {
		fmt.Fprintf(h, "%s\x00%s\x00", s.ID, s.Prose)
	}
	key := hex.EncodeToString(h.Sum(nil))[:16]
	st = loadCross(opt.Out)
	if st.Key == key {
		return st, adopting, earlier, true
	}
	if opt.MaxCost > 0 && res.Usage.CostUSD >= opt.MaxCost {
		return st, adopting, earlier, false
	}
	if opt.Log != nil {
		fmt.Fprintf(opt.Log, "review: reading the %d requirements' texts for contradictions ...\n", len(opt.Adopt)+len(earlier))
	}
	// The glossary an importer appends is the same under every
	// requirement of a spec: the review reads the requirement.
	stmts := append(slices.Clone(opt.Adopt), earlier...)
	text := map[string]string{}
	for i, s := range stmts {
		text[s.ID] = s.Prose
		stmts[i].Prose, _, _ = strings.Cut(s.Prose, "\nGlossary: ")
	}
	batches, _ := review.Plan(stmts, 150000)
	passes := make([]*review.Result, 2)
	if opt.MaxCost > 0 {
		// Under a spending limit the passes run one after the other, so
		// the second starts only if the first left something to spend.
		for pass := range passes {
			passes[pass] = review.Batches(ctx, opt.Clients[0], batches, nil, 1, nil)
		}
	} else {
		var wg sync.WaitGroup
		for pass := range passes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				passes[pass] = review.Batches(ctx, opt.Clients[0], batches, nil, 2, nil)
			}()
		}
		wg.Wait()
	}
	var found []review.Finding
	seen := map[string]bool{}
	failed := 0
	for _, rs := range passes {
		res.Usage.Add(rs.Usage)
		failed += len(rs.Failed)
		for _, f := range rs.Findings {
			ids := slices.Clone(f.StatementIDs)
			sort.Strings(ids)
			if k := strings.Join(ids, "\x00"); !seen[k] {
				seen[k] = true
				found = append(found, f)
			}
		}
	}
	if failed > 0 && len(found) == 0 {
		res.Warnings = append(res.Warnings, "review of the texts: the model call failed; contradictions are those the rules show")
		return st, adopting, earlier, false
	}
	st = crossState{Key: key, Findings: found, Steered: map[string]bool{}, Notes: map[string]crossNote{}}
	if opt.Log != nil {
		fmt.Fprintf(opt.Log, "review: %d possible contradiction(s) in the texts\n", len(found))
	}
	if opt.steerFirst {
		// Before any reading: every finding steers the first one.
		for _, f := range found {
			ids := slices.DeleteFunc(slices.Clone(f.StatementIDs), func(id string) bool { return text[id] == "" })
			sort.Strings(ids)
			ids = slices.Compact(ids)
			if len(ids) < 2 || !slices.ContainsFunc(ids, func(id string) bool { return adopting[id] }) {
				continue
			}
			st.Steered[strings.Join(ids, "\x00")] = true
			for _, id := range ids {
				if !adopting[id] {
					continue
				}
				var others []string
				for _, o := range ids {
					if o != id {
						t, _, _ := strings.Cut(text[o], "\nGlossary: ")
						others = append(others, o+": "+oneLine(strings.Join(strings.Fields(t), " "), 160))
					}
				}
				note := steerNote(others, f.Situation, false)
				if old, ok := st.Notes[id]; ok {
					note = old.Note + " " + strings.TrimPrefix(note, "review: ")
				}
				st.Notes[id] = crossNote{Hash: translate.TextHash(text[id]), Note: note}
			}
		}
	}
	if err := st.save(opt.Out); err != nil {
		return st, adopting, earlier, false
	}
	return st, adopting, earlier, true
}

// steerNote tells the translation what a review of the texts saw between
// this requirement and others. again says an earlier reading did not show it.
func steerNote(others []string, situation string, again bool) string {
	before := ""
	if again {
		before = " The earlier rules of the two did not meet."
	}
	return fmt.Sprintf("review: read by their texts alone, this requirement and another may not both hold. The other: %s. The situation: %s.%s Read this requirement exactly as its text says: keep every condition and qualifier it states (\"only\", \"automatically\", a named role or case), write a rule for each thing it requires, and use for each fact the two share one symbol, the one the vocabulary has for it. Do not bend what the requirement says to make a contradiction, or to avoid one: if the two do not contradict as written, the rules should not either.", strings.Join(others, " | "), strings.TrimSpace(situation), before)
}
