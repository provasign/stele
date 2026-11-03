package change

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// A Question is a decision a change needs, put in the spec's words for the
// coding agent to ask the owner (D-058). The owner never reads Stele's files.
type Question struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // same_term | creates | conflict | self_conflict | meaning_change | approve
	// AskOwner is false for a question the agent answers itself: Stele's own
	// misreading, re-read once before the owner is asked.
	AskOwner     bool          `json:"ask_owner"`
	Text         string        `json:"text"`
	Options      []Option      `json:"options"`
	Requirements []Requirement `json:"requirements,omitempty"`
	// Example is a situation in which the requirements disagree.
	Example string `json:"example,omitempty"`
	// Detail is for the agent: the terms or rules behind the question.
	Detail string `json:"detail,omitempty"`
	// Recommended is the choice the check suggests, and Why says why in
	// the spec's words; the agent offers it first.
	Recommended string `json:"recommended,omitempty"`
	Why         string `json:"why,omitempty"`
	// Code is what the project's code does in a conflict's situation.
	Code     *Code  `json:"code,omitempty"`
	Term     string `json:"term,omitempty"`
	Existing string `json:"existing,omitempty"`
	Link     string `json:"world_fact,omitempty"`
	// Statements are the statements an answer applies to: the ones the
	// change translates, which are read again with the answer as a note.
	Statements []string `json:"statements,omitempty"`

	// For grouping conflicts: the step, the statements and world facts.
	unit         string
	stmts, links []string
	rules        []string // the clashing rules
}

// Option is one answer to a question.
type Option struct {
	Choice string `json:"choice"`
	Label  string `json:"label"`
	Then   string `json:"then"` // what happens, for the agent
	// Note marks an option whose answer needs the owner's words.
	Note bool `json:"needs_note,omitempty"`
}

// Requirement names a statement the way the spec tool does.
type Requirement struct {
	ID         string `json:"id"`
	Capability string `json:"capability,omitempty"`
	Title      string `json:"title"`
	Where      string `json:"where"` // ADDED in this change, MODIFIED in this change, current spec
	Source     string `json:"source,omitempty"`
	Text       string `json:"text,omitempty"`
	// Reading is how Stele read the requirement, one line per rule, and
	// Findings what its own checks found in that reading, for the approval.
	Reading  []string `json:"reading,omitempty"`
	Findings []string `json:"findings,omitempty"`
}

// answersFile holds the owner's answers for one change.
const answersFile = "answers.json"

// Answer is one recorded answer.
type Answer struct {
	Question string `json:"question"`
	Kind     string `json:"kind"`
	Choice   string `json:"choice"`
	Note     string `json:"note,omitempty"`
	By       string `json:"by,omitempty"`
	Term     string `json:"term,omitempty"`
	Existing string `json:"existing,omitempty"`
	Link     string `json:"world_fact,omitempty"`
	// Statements maps each statement the answer applies to to the hash of
	// its text when answered: an edited statement no longer gets the note.
	Statements map[string]string `json:"statements,omitempty"`
}

// LoadAnswers reads a change's recorded answers.
func LoadAnswers(dir string) ([]Answer, error) {
	src, err := os.ReadFile(filepath.Join(dir, answersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var as []Answer
	if err := json.Unmarshal(src, &as); err != nil {
		return nil, fmt.Errorf("%s: %w", answersFile, err)
	}
	return as, nil
}

// SaveAnswer records an answer, replacing an earlier one to the same question.
func SaveAnswer(dir string, a Answer) error {
	as, err := LoadAnswers(dir)
	if err != nil {
		return err
	}
	// A note for the translator adds to the earlier ones; any other answer
	// replaces the earlier answer to its question.
	as = slices.DeleteFunc(as, func(x Answer) bool {
		return x.Question == a.Question && (!carriesNote(x) || !carriesNote(a) || x.Choice == a.Choice && x.Note == a.Note)
	})
	as = append(as, a)
	js, err := json.MarshalIndent(as, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, answersFile), append(js, '\n'), 0o644)
}

// carriesNote reports whether an answer is a note for the translator.
func carriesNote(a Answer) bool {
	return a.Choice == "misread" || a.Choice == "retranslate" || a.Choice == "impossible" || a.Kind == "reread"
}

// notes are the answers' notes for a statement as it is now written.
func notes(as []Answer, s translate.Statement) []string {
	var out []string
	h := translate.TextHash(s.Prose)
	for _, a := range as {
		if a.Statements[s.ID] != h {
			continue
		}
		switch {
		case a.Kind == "same_term" && a.Choice == "same":
			out = append(out, fmt.Sprintf("review: the owner says %s means the same as the existing %s; use %s and do not propose %s.", a.Term, a.Existing, a.Existing, a.Term))
		case a.Kind == "reread":
			out = append(out, "review: the owner says an earlier reading of this requirement was wrong: "+a.Note)
		case a.Kind == "conflict" && a.Choice == "impossible":
			out = append(out, "review: the owner says this situation cannot happen, so the requirements need not hold together in it: "+a.Note+". Where the requirement's own words allow, read its conditions so they exclude it.")
		case (a.Kind == "conflict" || a.Kind == "self_conflict") && a.Choice == "misread":
			out = append(out, "review: the owner says an earlier reading of this requirement was wrong: "+a.Note)
		case a.Kind == "self_conflict" && a.Choice == "retranslate":
			out = append(out, "review: an earlier reading of this requirement gave rules that cannot all hold together ("+a.Note+"). Read the requirement again so its rules agree with each other and with its text.")
		}
	}
	return out
}

// answered reports whether the question has an answer that settles it.
func answered(as []Answer, q Question) bool {
	for _, a := range as {
		if a.Question != q.ID {
			continue
		}
		switch a.Kind {
		case "same_term":
			return a.Choice == "different"
		case "meaning_change":
			return a.Choice == "intended"
		case "renamed":
			return true // "same" turns the pair into a rename; "different" leaves it a removal and an addition
		case "task":
			return true // "task" sets the request aside; "requirement" keeps it, unchecked
		}
	}
	return false
}

// retried reports whether every statement has already been re-read once for
// contradicting itself, with its current text.
func retried(as []Answer, stmts []string, hash func(string) string) bool {
	for _, id := range stmts {
		ok := false
		for _, a := range as {
			if a.Kind == "self_conflict" && carriesNote(a) && a.Statements[id] == hash(id) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func qid(kind string, parts ...string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + strings.Join(parts, "\x00")))
	return fmt.Sprintf("%s-%x", strings.Split(kind, "_")[0], h[:4])
}

// questions builds what the change needs decided.
func questions(res *Result, head *ir.Registry, c *cache, known map[string][]string, as []Answer) []Question {
	inChange := map[string]bool{}
	for _, id := range append(append([]string{}, res.Added...), res.Modified...) {
		inChange[id] = true
	}
	req := func(id string) Requirement {
		r := Requirement{ID: id, Title: id, Where: "current spec", Source: res.Sources[id], Text: res.Statements[id]}
		if cap, title, ok := strings.Cut(id, "/"); ok {
			r.Capability, r.Title = cap, title
		}
		if !strings.ContainsRune(r.Title, ' ') && strings.ContainsAny(r.Title, "0123456789") && r.Text != "" {
			// A bare code (FR-001, R1.2) repeats across features: name the
			// requirement by its own first line.
			line, rest, _ := strings.Cut(r.Text, "\n")
			r.Title = oneLine(line, 80)
			if rest != "" && !strings.Contains(line, title(id)) && r.Capability != "intent" {
				// A heading several criteria share (Kiro's "Requirement 2:
				// ..."): add the criterion's label.
				r.Title += " (" + title(id) + ")"
			}
		}
		for _, a := range res.Added {
			if a == id {
				r.Where = "ADDED in this change"
			}
		}
		for _, m := range res.Modified {
			if m == id {
				r.Where = "MODIFIED in this change"
			}
		}
		return r
	}
	hash := func(id string) string { return translate.TextHash(res.Statements[id]) }
	var qs []Question

	// New terms that may name something the spec already has.
	for _, f := range c.Flags {
		if !slices.Contains(res.NewTerms, f.Symbol) {
			continue
		}
		var readers []string
		for id := range inChange {
			for _, cd := range c.Entries[id].Cands {
				if mentions(cd, f.Symbol) && !slices.Contains(readers, id) {
					readers = append(readers, id)
				}
			}
		}
		sort.Strings(readers)
		if len(readers) == 0 {
			continue
		}
		if a, b := attrNamed(head, f.Symbol), attrNamed(head, f.Similar); a != nil && b != nil && a.Kind != b.Kind {
			// A yes-or-no and a number (say) cannot be one fact: both names
			// are kept, and nobody is asked what the types already say.
			continue
		}
		q := Question{ID: qid("same_term", f.Symbol, f.Similar), Kind: "same_term", AskOwner: true, Term: f.Symbol, Existing: f.Similar, Statements: readers,
			Detail: f.Reason}
		titles := make([]string, len(readers))
		for i, id := range readers {
			q.Requirements = append(q.Requirements, req(id))
			titles[i] = fmt.Sprintf("%q", req(id).Title)
		}
		newWords, oldWords := Words(f.Symbol, meaningOf(head, f.Symbol)), Words(f.Similar, meaningOf(head, f.Similar))
		if slices.Contains(res.NewTerms, f.Similar) {
			// Both names are Stele's, from this change's own requirements:
			// the agent can tell from their text.
			var users []string
			for id := range inChange {
				for _, cd := range c.Entries[id].Cands {
					if mentions(cd, f.Similar) && !slices.Contains(users, id) && !slices.Contains(readers, id) {
						users = append(users, id)
					}
				}
			}
			sort.Strings(users)
			for _, id := range users {
				q.Requirements = append(q.Requirements, req(id))
			}
			q.AskOwner = false
			q.Text = fmt.Sprintf("Stele named two things in this change: %s (in %s) and %s. Are they the same thing?", newWords, strings.Join(titles, " and "), oldWords)
			q.Options = []Option{
				{Choice: "same", Label: "The same thing", Then: "Stele reads these requirements again with one name; run `stele change` again"},
				{Choice: "different", Label: "Different things", Then: "both names are kept"},
			}
			if !answered(as, q) {
				qs = append(qs, q)
			}
			continue
		}
		var others []string
		for _, id := range known[f.Similar] {
			if !slices.Contains(readers, id) {
				others = append(others, id)
			}
		}
		switch {
		case len(others) == 0 && len(known[f.Similar]) > 0:
			// Only the requirement's own earlier version used the old term.
			q.Text = fmt.Sprintf("Before this change, %s spoke of %s; it now speaks of %s. Are they the same thing?", strings.Join(titles, " and "), oldWords, newWords)
		default:
			q.Text = fmt.Sprintf("%s in this change speaks of %s. The current spec already has %s", strings.Join(titles, " and "), newWords, oldWords)
			if len(others) > 0 {
				q.Text += fmt.Sprintf(", used by %q", req(others[0]).Title)
				for _, id := range others[:min(2, len(others))] {
					q.Requirements = append(q.Requirements, req(id))
				}
			}
			q.Text += ". Are they the same thing?"
		}
		q.Options = []Option{
			{Choice: "same", Label: "The same thing", Then: "Stele reads these requirements again using the existing term; run `stele change` again"},
			{Choice: "different", Label: "Different things", Then: "the new term is kept"},
			{Choice: "reword", Label: "Let me reword the requirement", Then: "edit the requirement so it says which one it means, then run `stele change` again"},
		}
		if !answered(as, q) {
			qs = append(qs, q)
		}
	}

	g := res.Gate
	if g != nil && g.Check != nil {
		stmtOf := func(rule string) string {
			if x := head.Rule(rule); x != nil {
				return x.StatementID
			}
			return ""
		}
		for _, cf := range g.Check.Conflicts {
			// A clash within one step that a state invariant takes part in
			// may only be an earlier instance carrying over. If the step
			// creates its entity, nothing carries over: the agent asks the
			// spec, not the owner, whether it does.
			if op := head.Operations[cf.Unit]; op != nil && !slices.Contains(op.Entity.CreatedBy, op.Name) && stateInvariantIn(head, cf.Rules) {
				q := Question{ID: qid("creates", op.Name), Kind: "creates", Term: op.Name, Existing: op.Entity.Name}
				if !existing(as, q.ID) {
					if !slices.ContainsFunc(qs, func(x Question) bool { return x.ID == q.ID }) {
						q.Text = fmt.Sprintf("Does %s create the %s, so that it never runs on one that already exists?", strings.Trim(Words(op.Name, ""), `"`), split(op.Entity.Name))
						q.Detail = "Stele found rules that only clash if this step runs on an existing " + op.Entity.Name + ". Decide from the spec (and the code): answer creates if the step makes a new one."
						q.Options = []Option{
							{Choice: "creates", Label: "Yes, it creates it", Then: "Stele records that the step creates the " + split(op.Entity.Name) + "; run `stele change` again"},
							{Choice: "existing", Label: "No, it can run on an existing one", Then: "the clashes are asked as conflicts; run `stele change` again"},
						}
						qs = append(qs, q)
					}
					continue
				}
			}
			var stmts, links []string
			for _, r := range cf.Rules {
				if s := stmtOf(r); s == "" {
					links = append(links, r)
				} else if !slices.Contains(stmts, s) {
					stmts = append(stmts, s)
				}
			}
			sort.Strings(stmts)
			var mine []string
			for _, s := range stmts {
				if inChange[s] {
					mine = append(mine, s)
				}
			}
			example := plainSituation(checker.Situation(head, cf.Counterexample))
			if cf.Unit != "" && cf.Unit != "state" {
				// The clash is in one step: say which.
				step := strings.Trim(Words(cf.Unit, ""), `"`) + " runs"
				if example != "" {
					step += " and "
				}
				example = step + example
			}
			var rules []string
			for _, r := range cf.Rules {
				if x := head.Rule(r); x != nil {
					rules = append(rules, r+": "+english(x))
				}
			}
			q := Question{Example: example, Statements: mine, Detail: strings.Join(rules, "\n"), rules: cf.Rules}
			for _, s := range stmts {
				q.Requirements = append(q.Requirements, req(s))
			}
			if len(stmts) == 1 && len(links) == 0 {
				// One question per requirement, however many ways its rules clash.
				if k := slices.IndexFunc(qs, func(x Question) bool { return x.Kind == "self_conflict" && x.Requirements[0].ID == stmts[0] }); k >= 0 {
					qs[k].Detail += "\n" + q.Detail
					continue
				}
				q.ID, q.Kind = qid("self_conflict", stmts[0], hash(stmts[0])), "self_conflict"
				q.AskOwner = retried(as, stmts, hash)
				q.Text = fmt.Sprintf("Stele's reading of %q contradicts itself", q.Requirements[0].Title)
				if example != "" {
					q.Text += " (when " + example + ")"
				}
				q.Text += ". This is usually Stele misreading the requirement, not the requirement being wrong."
				if q.AskOwner {
					q.Text = fmt.Sprintf("Read twice, %q still comes out contradicting itself", q.Requirements[0].Title)
					if example != "" {
						q.Text += " (when " + example + ")"
					}
					q.Text += ". Does the requirement say two things that cannot both hold?"
				}
				q.Options = []Option{
					{Choice: "retranslate", Label: "Read it again", Then: "Stele reads the requirement again with the contradiction as a note; run `stele change` again"},
					{Choice: "reword", Label: "The requirement contradicts itself: reword it", Then: "edit the requirement, then run `stele change` again"},
				}
				if q.AskOwner {
					q.Options = []Option{
						{Choice: "misread", Label: "Stele misread it", Then: "Stele reads the requirement again with the owner's note; run `stele change` again", Note: true},
						q.Options[1],
					}
				}
				qs = append(qs, q)
				continue
			}
			q.ID, q.Kind, q.AskOwner = qid("conflict", append(append([]string{}, stmts...), links...)...), "conflict", true
			q.unit, q.stmts, q.links = cf.Unit, stmts, links
			titles := make([]string, len(q.Requirements))
			for i, r := range q.Requirements {
				titles[i] = fmt.Sprintf("%q (%s)", r.Title, strings.ToLower(r.Where))
			}
			switch {
			case len(links) > 0 && len(titles) > 0:
				x := head.Rule(links[0])
				q.Link = links[0]
				q.Text = fmt.Sprintf("%s cannot hold given what Stele assumed about the world: %s", strings.Join(titles, " and "), strings.TrimPrefix(x.Prose, "World fact: "))
			default:
				q.Text = strings.Join(titles, " and ") + " cannot both hold"
			}
			if example != "" {
				q.Text += ", for example when " + example
			}
			q.Text += ". Which is right?"
			q.Options = []Option{
				{Choice: "change", Label: "Change the requirement in this change", Then: "ask the owner what it should say, edit the change's spec, then run `stele change` again"},
			}
			if len(mine) < len(stmts) {
				q.Options = append(q.Options, Option{Choice: "update_old", Label: "The current spec is out of date: update it in this change", Then: "add a MODIFIED (or REMOVED) requirement for the older one to this change, then run `stele change` again"})
			}
			q.Options = append(q.Options,
				Option{Choice: "exception", Label: "One is an exception to the other: say so in the spec", Then: "reword the requirements so the exception is written down, then run `stele change` again"},
				Option{Choice: "impossible", Label: "That situation cannot happen", Then: "Stele reads the requirements in this change again knowing that; run `stele change` again"},
				Option{Choice: "misread", Label: "Stele misread a requirement", Then: "Stele reads the requirements in this change again with the owner's note; run `stele change` again", Note: true})
			if len(links) > 0 && strings.HasPrefix(links[0], "world-") {
				q.Options = append(q.Options, Option{Choice: "wrong_fact", Label: "That assumption about the world is wrong", Then: "Stele drops it; run `stele change` again"})
			}
			if res.Format == "shale" {
				shaleOptions(q.Options)
			}
			n, why := res.narrow[strings.Join(stmts, "\x00")], "%q applies only in situations %q also covers, so it reads as an exception to it"
			if n == "" && len(stmts) == 2 {
				// Overlapping conditions do not nest, but one requirement
				// may say what happens when the other cannot (ISSUES 56).
				n, why = fallbackOf(req(stmts[0]).Text, req(stmts[1]).Text, stmts), "%q says what happens when %q cannot be followed, so it reads as an exception to it"
			}
			if n != "" && len(stmts) == 2 && len(links) == 0 {
				other := stmts[0]
				if other == n {
					other = stmts[1]
				}
				q.Recommended = "exception"
				q.Why = fmt.Sprintf(why, req(n).Title, req(other).Title)
				k := slices.IndexFunc(q.Options, func(o Option) bool { return o.Choice == "exception" })
				q.Options = append([]Option{q.Options[k]}, slices.Delete(slices.Clone(q.Options), k, k+1)...)
			}
			if len(mine) == 0 { // only the older requirements conflict: nothing in this change to re-read
				q.Options = slices.DeleteFunc(q.Options, func(o Option) bool { return o.Choice == "misread" || o.Choice == "change" || o.Choice == "impossible" })
			}
			qs = append(qs, q)
		}
	}

	qs = groupConflicts(qs, head, req)

	// Vocabulary the change alters under rules already approved.
	if g != nil {
		made := creators(as)
		for _, ch := range g.NeedsApproval {
			if len(ch.Affected) == 0 {
				continue
			}
			// What a creating step newly writes, what the change's rules make
			// an existing step write, and who then sets those values, follow
			// from the answer and the change's own text.
			ent, _, _ := strings.Cut(ch.Symbol, ".")
			inferred := false
			for _, ws := range res.InferredWrites {
				inferred = inferred || slices.Contains(ws, ch.Symbol)
			}
			if (ch.Kind == "writes-changed" && (slices.Contains(made[ent], ch.Symbol) || res.InferredWrites[ch.Symbol] != nil)) || (ch.Kind == "controller-changed" && (len(made[ent]) > 0 || inferred)) {
				continue
			}
			q := Question{ID: qid("meaning_change", ch.Symbol, ch.Detail), Kind: "meaning_change", AskOwner: true, Term: ch.Symbol, Detail: ch.Kind + ": " + ch.Detail}
			seen := map[string]bool{}
			for _, r := range ch.Affected {
				if s := head.Rule(r); s != nil && s.StatementID != "" && !seen[s.StatementID] {
					seen[s.StatementID] = true
					q.Requirements = append(q.Requirements, req(s.StatementID))
				}
			}
			q.Text = fmt.Sprintf("This change alters %s (%s), which changes how %d approved requirement(s) read. Is that intended?", Words(ch.Symbol, ""), ch.Detail, len(q.Requirements))
			q.Options = []Option{
				{Choice: "intended", Label: "Yes, intended", Then: "kept"},
				{Choice: "reword", Label: "No: reword the change", Then: "edit the change's spec, then run `stele change` again"},
			}
			if !answered(as, q) {
				qs = append(qs, q)
			}
		}
	}

	// A requirement removed and one added that read alike: the same
	// requirement renamed and edited without a RENAMED record? Until the
	// owner says, the old rules retire and the new ones arrive as strangers,
	// and the decisions that cite the old rules go stale.
	for _, gs := range res.RenameGuesses {
		q := Question{ID: qid("renamed", gs.From, gs.To), Kind: "renamed", AskOwner: true, Term: gs.From, Existing: gs.To,
			Requirements: []Requirement{req(gs.From), req(gs.To)},
			Detail:       fmt.Sprintf("%.0f%% of the words in common", gs.Similarity*100)}
		q.Requirements[0].Where = "REMOVED in this change"
		q.Text = fmt.Sprintf("This change removes %q and adds %q, and the two read much alike. Is %q the same requirement, renamed and edited?", req(gs.From).Title, req(gs.To).Title, req(gs.To).Title)
		q.Recommended, q.Why = "same", "its rules then keep their identity and history: a new version under the old name, and the decisions that cite them stay valid"
		q.Options = []Option{
			{Choice: "same", Label: "Yes, the same requirement renamed", Then: "run `stele change` again: its rules get a new version under their old IDs instead of retiring"},
			{Choice: "different", Label: "No, one is removed and the other is new", Then: "the old rules retire and the new requirement is translated afresh"},
		}
		if !answered(as, q) {
			qs = append(qs, q)
		}
	}

	// A request that gives Stele nothing to check is often a one-off task,
	// not something the product must keep ("write the review", "set up the
	// tracker"): approving it would record a requirement that protects
	// nothing (a developer trial, 2026-11-02). The owner says which.
	if res.Format == "shale" {
		for _, id := range append(append([]string{}, res.Added...), res.Modified...) {
			if _, failed := res.Failed[id]; failed || len(res.Rules[id]) > 0 || !strings.HasPrefix(id, "intent/") {
				continue
			}
			q := Question{ID: qid("task", id, translate.TextHash(res.Statements[id])), Kind: "task", AskOwner: true,
				Requirements: []Requirement{req(id)}, Statements: []string{id}}
			q.Text = fmt.Sprintf("Stele found nothing to check in the request %q. Is it a requirement the product must keep, or a one-off task?", req(id).Title)
			q.Options = []Option{
				{Choice: "task", Label: "A one-off task, not a requirement", Then: "Stele sets the request aside in .stele/retired-intents.yaml and it leaves the change; run `stele change` again"},
				{Choice: "requirement", Label: "A requirement, though Stele cannot check it", Then: "it goes to the approval as a requirement Stele records but does not check; run `stele change` again"},
			}
			if !answered(as, q) {
				qs = append(qs, q)
			}
		}
	}

	// The approval, once nothing else is open.
	if g != nil && len(g.Errors) == 0 && g.Accepted && len(qs) == 0 && len(g.Unapproved) > 0 && len(res.Failed) == 0 {
		var checked []string
		for id := range inChange {
			if len(res.Rules[id]) > 0 {
				checked = append(checked, id)
			}
		}
		sort.Strings(checked)
		q := Question{ID: qid("approve", g.Unapproved...), Kind: "approve", AskOwner: true}
		flagged := 0
		for _, id := range checked {
			r := req(id)
			for _, rid := range res.Rules[id] {
				if x := head.Rule(rid); x != nil {
					r.Reading = append(r.Reading, english(x))
				}
			}
			r.Findings = res.Findings[id]
			if len(r.Findings) > 0 {
				flagged++
			}
			q.Requirements = append(q.Requirements, r)
		}
		if !res.first {
			q.Text = fmt.Sprintf("%d requirement(s) in this change are checked and agree with the approved spec", len(checked))
		} else {
			// The first change: there is nothing approved to agree with yet.
			q.Text = fmt.Sprintf("%d requirement(s) in this change are checked and agree with each other; nothing is approved yet, so these are the first", len(checked))
		}
		if flagged > 0 {
			q.Text += fmt.Sprintf("; Stele's reading of %d may say less or more than the text (see findings)", flagged)
			q.Recommended, q.Why = "show", "look at the flagged readings before approving them"
		}
		if n := len(res.Unchecked); n > 0 {
			var titles []string
			for _, id := range sortedKeys(res.Unchecked) {
				titles = append(titles, fmt.Sprintf("%q", req(id).Title))
			}
			q.Text += fmt.Sprintf("; %d could not be checked (%s)", n, strings.Join(titles, ", "))
		}
		if len(res.NewTerms) > 0 {
			q.Text += fmt.Sprintf(". The change adds %d new term(s)", len(res.NewTerms))
		}
		if n := len(res.NewLessons); n > 0 {
			q.Text += fmt.Sprintf(". Your answers also teach Stele %d lesson(s) about this project, saved in the registry with the change: %s", n, strings.Join(res.NewLessons, "; "))
		}
		q.Text += ". Approve this change?"
		q.Detail = strings.Join(g.Unapproved, "\n")
		q.Options = []Option{
			{Choice: "approve", Label: "Approve", Then: "Stele writes the change's rules into the registry and records the approval"},
			{Choice: "show", Label: "Show me how Stele read them first", Then: "show the requirements with Stele's reading (report.md, \"Rules to approve\"), then ask again"},
			{Choice: "not_yet", Label: "Not yet", Then: "nothing is recorded"},
		}
		qs = append(qs, q)
	}
	return qs
}

// mentions reports whether a candidate's text names the attribute's last segment.
func mentions(c translate.Candidate, symbol string) bool {
	name := symbol[strings.LastIndexByte(symbol, '.')+1:]
	for _, t := range []string{c.When, c.Assert, c.On, c.Operation} {
		for _, w := range strings.FieldsFunc(t, func(r rune) bool { return !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)) }) {
			if w == name {
				return true
			}
		}
	}
	return false
}

func meaningOf(reg *ir.Registry, symbol string) string {
	ent, name, _ := strings.Cut(symbol, ".")
	if e := reg.Entities[ent]; e != nil {
		if a := e.Attrs[name]; a != nil {
			return a.Meaning
		}
	}
	return ""
}

// Words names a symbol for a reader of the spec: its meaning when it has
// one, else its name split into words ("Cluster.flushRecorded" is "cluster:
// flush recorded").
func Words(symbol, meaning string) string {
	if meaning != "" {
		return fmt.Sprintf("%q", meaning)
	}
	ent, name, ok := strings.Cut(symbol, ".")
	if !ok {
		return fmt.Sprintf("%q", split(symbol))
	}
	return fmt.Sprintf("%q", split(ent)+": "+split(name))
}

func split(s string) string { return checker.SplitName(s) }

// plainSituation renders "Cluster.flushRecorded=false" as "cluster: flush recorded is false".
func plainSituation(s []string) string { return checker.Plain(s) }

// english is a short plain reading of a rule.
func english(x *ir.Rule) string {
	var parts []string
	switch x.Kind {
	case ir.Permission, ir.Precondition:
		parts = append(parts, fmt.Sprintf("%s %s %s", x.Kind, x.Effect, x.Operation))
	default:
		parts = append(parts, x.Kind)
		if x.On != "" {
			parts = append(parts, "on "+x.On)
		}
	}
	if x.When != nil {
		parts = append(parts, "when "+x.When.String())
	}
	if x.Assert != nil {
		parts = append(parts, "then "+x.Assert.String())
	}
	return strings.Join(parts, " ")
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// stateInvariantIn reports whether one of the rules is an invariant that
// holds in every state, not only after one operation.
func stateInvariantIn(reg *ir.Registry, rules []string) bool {
	for _, r := range rules {
		if x := reg.Rule(r); x != nil && x.Kind == ir.Invariant && x.On == "" {
			return true
		}
	}
	return false
}

// existing reports whether the owner or agent said the step can run on an
// existing instance.
func existing(as []Answer, id string) bool {
	return slices.ContainsFunc(as, func(a Answer) bool { return a.Question == id && a.Choice == "existing" })
}

// creators are the answers that say a step creates its entity: entity to operations.
func creators(as []Answer) map[string][]string {
	out := map[string][]string{}
	for _, a := range as {
		if a.Kind == "creates" && a.Choice == "creates" && !slices.Contains(out[a.Existing], a.Term) {
			out[a.Existing] = append(out[a.Existing], a.Term)
		}
	}
	return out
}

// RemoveAnswer deletes a recorded answer; it reports whether there was one.
func RemoveAnswer(dir, question string) (bool, error) {
	as, err := LoadAnswers(dir)
	if err != nil {
		return false, err
	}
	n := len(as)
	as = slices.DeleteFunc(as, func(x Answer) bool { return x.Question == question })
	if len(as) == n {
		return false, nil
	}
	js, err := json.MarshalIndent(as, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(filepath.Join(dir, answersFile), append(js, '\n'), 0o644)
}

// groupConflicts merges the conflicts one requirement of the change has,
// within one step, with several others: usually one missing exception,
// which one answer settles (ISSUES 60). Each conflict joins the group of
// its requirement in the change that the most conflicts share.
func groupConflicts(qs []Question, head *ir.Registry, req func(string) Requirement) []Question {
	count := map[string]int{}
	for _, q := range qs {
		if q.Kind == "conflict" {
			for _, id := range q.Statements {
				count[q.unit+"\x00"+id]++
			}
		}
	}
	anchor := func(q Question) string {
		best := ""
		for _, id := range q.Statements {
			if k := q.unit + "\x00" + id; best == "" || count[k] > count[best] || count[k] == count[best] && k < best {
				best = k
			}
		}
		return best
	}
	groups := map[string][]int{}
	var order []string
	for i, q := range qs {
		if q.Kind != "conflict" || len(q.Statements) == 0 {
			continue
		}
		k := anchor(q)
		if count[k] < 2 {
			continue
		}
		if groups[k] == nil {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	merged := map[int]bool{}
	first := map[int]Question{} // a group's question, at its first member's place
	var out []Question
	for _, k := range order {
		idx := groups[k]
		if len(idx) < 2 {
			continue
		}
		_, x, _ := strings.Cut(k, "\x00")
		g := Question{Kind: "conflict", AskOwner: true, Example: qs[idx[0]].Example, unit: qs[idx[0]].unit}
		var others, facts, ids, stmts, recs, whys []string
		seen := map[string]bool{x: true}
		g.Requirements = append(g.Requirements, req(x))
		for _, i := range idx {
			merged[i] = true
			q := qs[i]
			for _, id := range q.stmts {
				if !seen[id] {
					seen[id] = true
					g.Requirements = append(g.Requirements, req(id))
					others = append(others, fmt.Sprintf("%q (%s)", req(id).Title, strings.ToLower(req(id).Where)))
				}
			}
			for _, l := range q.links {
				if r := head.Rule(l); r != nil && !seen[l] {
					seen[l] = true
					facts = append(facts, fmt.Sprintf("%q", strings.TrimSuffix(strings.TrimPrefix(r.Prose, "World fact: "), ".")))
				}
			}
			for _, id := range q.Statements {
				if !slices.Contains(stmts, id) {
					stmts = append(stmts, id)
				}
			}
			if q.Recommended != "" {
				recs = append(recs, q.Recommended)
				whys = append(whys, q.Why)
			}
			ids = append(ids, append(append([]string{}, q.stmts...), q.links...)...)
			g.Detail = strings.TrimSpace(g.Detail + "\n" + q.Detail)
			g.stmts, g.links, g.rules = append(g.stmts, q.stmts...), append(g.links, q.links...), append(g.rules, q.rules...)
		}
		sort.Strings(ids)
		g.ID = qid("conflict", slices.Compact(ids)...)
		g.Statements = stmts
		var against []string
		if n := len(others); n > 1 {
			against = append(against, strings.Join(others[:n-1], ", ")+" and "+others[n-1])
		} else if n == 1 {
			against = append(against, others[0])
		}
		if len(facts) > 0 {
			against = append(against, "what Stele assumed about the world ("+strings.Join(facts, "; ")+")")
		}
		g.Text = fmt.Sprintf("%q (%s) cannot hold together with %s", req(x).Title, strings.ToLower(req(x).Where), strings.Join(against, " and with "))
		if g.Example != "" {
			g.Text += ", for example when " + g.Example
		}
		g.Text += fmt.Sprintf(". These %d clashes are one question. Which is right?", len(idx))
		// The options every merged question offered, in their order.
		for _, o := range qs[idx[0]].Options {
			all := true
			for _, i := range idx[1:] {
				all = all && slices.ContainsFunc(qs[i].Options, func(p Option) bool { return p.Choice == o.Choice })
			}
			if all && o.Choice != "wrong_fact" {
				g.Options = append(g.Options, o)
			}
		}
		// The members' recommendations carry over when they agree.
		if len(recs) > 0 && !slices.ContainsFunc(recs, func(r string) bool { return r != recs[0] }) &&
			slices.ContainsFunc(g.Options, func(o Option) bool { return o.Choice == recs[0] }) {
			recommend(&g, recs[0], strings.Join(slices.Compact(whys), "; "))
		}
		first[idx[0]] = g
	}
	for i, q := range qs {
		if g, ok := first[i]; ok {
			out = append(out, g)
		} else if !merged[i] {
			out = append(out, q)
		}
	}
	return out
}

// title is the last part of a statement ID: its label (FR-001, R2.5).
func title(id string) string { return id[strings.LastIndexByte(id, '/')+1:] }

var fallback = regexp.MustCompile(`(?i)\bfall(s|ing)? back\b|\bfallback\b|\b(if|when|where)\b[^.;]{0,80}\b(cannot (be )?(run|started|opened|reached|executed|loaded|launched|found)|(is|are) (unavailable|not available|not found|missing|absent))\b`)

// fallbackOf is the one of two statements whose text says what happens when
// something is unavailable or cannot run (a bare "fails" is not enough: "exit
// 0 when writes fail with a broken pipe" is no fallback) ("fall back to stdout when the pager
// cannot run"), if exactly one does.
func fallbackOf(a, b string, ids []string) string {
	fa, fb := fallback.MatchString(a), fallback.MatchString(b)
	switch {
	case fa && !fb:
		return ids[0]
	case fb && !fa:
		return ids[1]
	}
	return ""
}

// attrNamed is the attribute a symbol such as Rental.dayRateCharged names
// in reg, or nil.
func attrNamed(reg *ir.Registry, symbol string) *ir.Attr {
	i := strings.LastIndex(symbol, ".")
	if reg == nil || i < 0 {
		return nil
	}
	if e := reg.Entities[symbol[:i]]; e != nil {
		return e.Attrs[symbol[i+1:]]
	}
	return nil
}

// shaleOptions words a conflict's options for requests declared with
// `shale intent`: the evidence is append-only, so a request is corrected by
// declaring it again, and an older one is retired by the answer itself.
func shaleOptions(os []Option) {
	for i := range os {
		switch os[i].Choice {
		case "change":
			os[i].Label = "Change the new request"
			os[i].Then = "ask the owner what the request should say, declare it again with `shale intent \"<title>\" --body \"...\"`, then run `stele change` again"
		case "update_old":
			os[i].Label = "The earlier request is out of date: retire it"
			os[i].Then = "Stele retires the earlier request and its rules (" + "`.stele/retired-intents.yaml`" + ", committed with the change); run `stele change` again"
		case "exception":
			// An earlier request cannot be edited to name its exception, so
			// the new one states the whole rule and the earlier one retires.
			os[i].Label = "One is an exception to the other: restate them as one request"
			os[i].Then = "Stele retires the earlier request; declare this request again stating the whole rule with its exception (such as \"at most 5, or up to 10 during the program\") with `shale intent \"<title>\" --body \"...\"`, show the owner the wording, then run `stele change` again"
		}
	}
}

// Reading is how Stele reads a rule, in one line, as the approval question
// shows it.
func Reading(x *ir.Rule) string { return english(x) }
