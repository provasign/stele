package change

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/card"
	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// A Question is a decision a change needs, put in the spec's words for the
// coding agent to ask the owner (D-058). The owner never reads Stele's files.
type Question struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // same_term | creates | conflict | self_conflict | meaning_change | reported_write | approve
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
	// Links are all the world facts the conflict rests on, Facts what each
	// says (factKey): "that assumption is wrong" drops every one, and any
	// later reading that proposes one of them again (ISSUES 78).
	Links []string `json:"world_facts,omitempty"`
	Facts []string `json:"fact_keys,omitempty"`
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
	// Reading is how Stele read the requirement, one line per rule.
	// Findings are what its own checks found in that reading that the
	// owner decides on: a reading that says more than the text, or blocks
	// a step. NotChecked are the clauses the reading leaves out: recorded
	// and listed in the report, a count in the summary, and nothing the
	// owner can change by answering.
	Reading    []string `json:"reading,omitempty"`
	Findings   []string `json:"findings,omitempty"`
	NotChecked []string `json:"not_checked,omitempty"`
}

// OwnerFinding reports whether a finding of Stele's own checks is one the
// owner decides on at the approval: a reading broader than the text, or
// one that blocks a step, approves something the text does not say. A
// clause no rule checks leaves the reading weaker and nothing to decide;
// agents recited each one before every approval (a review, 2026-11-09).
func OwnerFinding(f string) bool {
	return strings.HasPrefix(f, "broader than the text: ") || strings.HasPrefix(f, "blocks a step: ")
}

// NotJudged reports whether a finding says Stele's check of its own
// reading did not run: the agent's to retry, not the owner's.
func NotJudged(f string) bool { return strings.HasPrefix(f, "not judged: ") }

// SplitFindings separates what the owner decides on from what the reading
// leaves unchecked, and counts the readings Stele could not double-check.
func SplitFindings(fs []string) (owner, notChecked []string, notJudged int) {
	for _, f := range fs {
		switch {
		case OwnerFinding(f):
			owner = append(owner, f)
		case NotJudged(f):
			notJudged++
		default:
			notChecked = append(notChecked, f)
		}
	}
	return owner, notChecked, notJudged
}

// answersFile holds the owner's answers for one change.
const answersFile = "answers.json"

// Answer is one recorded answer.
type Answer struct {
	Question string `json:"question"`
	Kind     string `json:"kind"`
	Choice   string `json:"choice"`
	Note     string `json:"note,omitempty"`
	// Quoted says the note is the owner's own words, as they said them.
	// Only such a note teaches Stele a lesson: a note the agent wrote, for
	// itself or to explain the owner's one-word answer, guides this
	// change's reading and is not the owner's (2026-11-07: the owner said
	// "1", and the agent's explanation was about to be saved as theirs).
	Quoted bool `json:"quoted,omitempty"`
	// Said is the owner's note as given, where Note adds to it what Stele
	// needs to act on it (the situation a "cannot happen" answer rules
	// out). A lesson keeps Said: the situation is the solver's slots.
	Said     string `json:"said,omitempty"`
	By       string `json:"by,omitempty"`
	Term     string `json:"term,omitempty"`
	Existing string `json:"existing,omitempty"`
	Link     string `json:"world_fact,omitempty"`
	// Links are all the world facts the conflict rests on, Facts what each
	// says (factKey): "that assumption is wrong" drops every one, and any
	// later reading that proposes one of them again (ISSUES 78).
	Links []string `json:"world_facts,omitempty"`
	Facts []string `json:"fact_keys,omitempty"`
	// Statements maps each statement the answer applies to to the hash of
	// its text when answered: an edited statement no longer gets the note.
	Statements map[string]string `json:"statements,omitempty"`
	// Over are the requirements a "precedence" answer gives way to.
	Over []string `json:"over,omitempty"`
}

// AgentReread reports whether the agent already had Stele read the
// statement again in this change, by a reread note or a "misread" answer,
// while its text was the one hashed. The agent gets one: a second note
// chased the first, each reread fixing what it was told and reading
// something else too wide (three runs, $3, twelve of twenty readings still
// flagged on 2026-11-07). After one, the clause stays as Stele reads it
// and the owner sees it at the approval.
func AgentReread(as []Answer, id, hash string) bool {
	for _, a := range as {
		if a.By == "agent" && (a.Kind == "reread" || a.Choice == "misread") && a.Statements[id] == hash {
			return true
		}
	}
	return false
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
	return a.Choice == "misread" || a.Choice == "retranslate" || a.Choice == "impossible" || a.Kind == "reread" || a.Kind == "free_setting" && a.Choice == "fixed"
}

// notes are the answers' notes for a statement as it is now written.
func notes(as []Answer, s translate.Statement) []string {
	var out []string
	h := translate.TextHash(s.Prose)
	for _, a := range as {
		was, ok := a.Statements[s.ID]
		if !ok {
			continue
		}
		// Said of an earlier wording, the note still says what the owner
		// meant: dropped, the rewording that was meant to clear a misreading
		// brought it back with every answer lost (a developer trial,
		// 2026-11-03). The translation judges whether it still applies.
		review := "review: "
		if was != h {
			review = "review (the owner said this of an earlier wording of this requirement; follow it where the current wording allows): "
		}
		switch {
		case a.Kind == "same_term" && a.Choice == "same":
			out = append(out, review+fmt.Sprintf("the owner says %s means the same as the existing %s; use %s and do not propose %s.", a.Term, a.Existing, a.Existing, a.Term))
		case a.Kind == "reread":
			out = append(out, review+"the owner says an earlier reading of this requirement was wrong: "+a.Note)
		case a.Kind == "conflict" && a.Choice == "impossible":
			out = append(out, review+"the owner says this situation cannot happen, so the requirements need not hold together in it: "+a.Note+". Where the requirement's own words allow, read its conditions so they exclude it.")
		case (a.Kind == "conflict" || a.Kind == "self_conflict") && a.Choice == "misread":
			out = append(out, review+"the owner says an earlier reading of this requirement was wrong: "+a.Note)
		case a.Kind == "conflict" && a.Choice == "precedence" && len(a.Over) > 0:
			out = append(out, review+fmt.Sprintf("the owner says this requirement takes precedence over %s where both apply: on each rule of this requirement that clashes with them, list %s under overrides. Keep every clause of this requirement as it reads.", strings.Join(a.Over, ", "), strings.Join(a.Over, ", ")))
		case a.Kind == "free_setting" && a.Choice == "fixed":
			out = append(out, review+"the owner says the value an earlier reading took from a new setting is not free: "+a.Note+". Write that value, or the approved term that holds it, instead of the setting.")
		case a.Kind == "self_conflict" && a.Choice == "retranslate":
			out = append(out, review+"an earlier reading of this requirement gave rules that cannot all hold together ("+a.Note+"). Read the requirement again so its rules agree with each other and with its text.")
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
		case "free_setting":
			return true // "fixed" rereads with the note; "setting" keeps it free
		case "restates":
			return true // "update_old" retires the earlier request; "both" keeps it
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
			line := card.Headline(r.Text)
			_, rest, _ := strings.Cut(r.Text, line)
			r.Title = oneLine(line, 80)
			if strings.TrimSpace(rest) != "" && !strings.Contains(line, title(id)) && r.Capability != "intent" {
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
			// A clash in a state no step reaches is set aside, but only for
			// an entity whose creating steps are known: otherwise its
			// instances may start in any state. A lent tool with no lent
			// date clashed with the loan history, though only lending makes
			// a tool lent and it sets the date (a developer trial,
			// 2026-11-03). The agent says, from the spec and the code, which
			// step makes a new one.
			if op, ops := head.Operations[cf.Unit], res.creators[conflictKey(cf)]; op != nil && len(ops) > 0 {
				q := Question{ID: qid("creator", op.Entity.Name), Kind: "creator", Existing: op.Entity.Name}
				if !slices.ContainsFunc(as, func(a Answer) bool { return a.Question == q.ID }) {
					if !slices.ContainsFunc(qs, func(x Question) bool { return x.ID == q.ID }) {
						q.Text = fmt.Sprintf("Which step makes a new %s?", split(op.Entity.Name))
						q.Detail = "Stele found rules that clash in a state of the " + op.Entity.Name + " that no step reaches if a new one starts with one of these steps; without knowing which, it must assume a new one can start in that state. Decide from the spec (and the code)."
						for _, name := range ops {
							q.Options = append(q.Options, Option{Choice: name, Label: strings.Trim(Words(name, ""), `"`), Then: "Stele records that the step creates the " + split(op.Entity.Name) + "; run `stele change` again"})
						}
						q.Options = append(q.Options, Option{Choice: "none", Label: "None of them: they exist from the start or come from outside", Then: "the clashes are asked as conflicts; run `stele change` again"})
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
				q.Link, q.Links = links[0], links
				var said []string
				for _, l := range links {
					if x := head.Rule(l); x != nil {
						said = append(said, strings.TrimPrefix(x.Prose, "World fact: "))
						q.Facts = append(q.Facts, ruleFactKey(x))
					}
				}
				q.Text = fmt.Sprintf("%s cannot hold given what Stele assumed about the world: %s", strings.Join(titles, " and "), strings.Join(said, "; "))
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
				label := "That assumption about the world is wrong"
				if len(links) > 1 {
					label = "Those assumptions about the world are wrong"
				}
				q.Options = append(q.Options, Option{Choice: "wrong_fact", Label: label, Then: "Stele drops it, retiring it with the approval if it was approved earlier; run `stele change` again"})
			}
			if res.Format == "shale" && len(mine) > 0 && len(mine) < len(stmts) {
				// Both stand, the new one prevailing where they meet: a
				// refusal added to an approved "can be lent when ..." no
				// longer means restating the whole earlier request (a
				// developer trial, 2026-11-03). Recorded as the rules'
				// overrides, as a text that says "regardless of" is (D-052).
				q.Options = append(q.Options, Option{Choice: "precedence", Label: "Both stand: the new request takes precedence where they meet", Then: "Stele reads the new request again giving it precedence over the earlier one where both apply; run `stele change` again"})
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

	// What an approved step is recorded as writing that its rules only
	// report: asked before the clashes in that step, which the extra write
	// may be what makes (a listing that could change any tool's name).
	var kept []Question
	for _, name := range slices.Sorted(maps.Keys(res.reportedWrites)) {
		op := head.Operations[name]
		for _, w := range res.reportedWrites[name] {
			q := Question{ID: qid("reported_write", name, w), Kind: "reported_write", Term: name, Existing: w}
			if slices.ContainsFunc(as, func(a Answer) bool { return a.Question == q.ID }) {
				continue
			}
			step := strings.Trim(Words(name, ""), `"`)
			value := strings.Trim(Words(w, meaningOf(head, w)), `"`)
			q.Text = fmt.Sprintf("Does %s change %s, or only report it?", step, value)
			var on []string
			for _, x := range head.Rules {
				if x.Live() && x.On == name {
					on = append(on, x.ID)
				}
			}
			sort.Strings(on)
			q.Detail = fmt.Sprintf("The registry records that %s writes %s, but every approved rule on the step (%s) only reports or tests it, and none says what it is after the step. A step that writes a value may leave it in any state, so the checks treat %s as able to change it. Decide from the spec (and the code).", name, w, strings.Join(on, ", "), name)
			q.Recommended, q.Why = "keeps", "no approved rule says the step changes it"
			q.Options = []Option{
				{Choice: "keeps", Label: "It only reports it: the step keeps the value", Then: "Stele records that " + step + " keeps it, and the owner approves that with the change; run `stele change` again"},
				{Choice: "changes", Label: "The step changes it", Then: "it stays a write; run `stele change` again"},
			}
			q.unit = op.Name
			kept = append(kept, q)
		}
	}
	if len(kept) > 0 {
		asked := map[string]bool{}
		for _, q := range kept {
			asked[q.unit] = true
		}
		qs = append(kept, slices.DeleteFunc(qs, func(q Question) bool { return (q.Kind == "conflict" || q.Kind == "self_conflict") && asked[q.unit] })...)
	}

	// Vocabulary the change alters under rules already approved.
	if g != nil {
		made, kept := creators(as), keeps(as)
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
			if (ch.Kind == "writes-changed" && (slices.Contains(made[ent], ch.Symbol) || res.InferredWrites[ch.Symbol] != nil || onlyKept(ch.Detail, kept[ch.Symbol]))) || (ch.Kind == "controller-changed" && (len(made[ent]) > 0 || inferred || keptAny(kept, ch.Symbol))) {
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

	// A new request that reads much like an approved one still in force is
	// usually that request restated with a change ("at most 2" for "at most
	// 3"). Rules that only narrow each other do not conflict, so without
	// asking, both would stay approved and the spec would say both (a
	// developer trial, 2026-11-03).
	if res.Format == "shale" {
		changed := map[string]bool{}
		for _, id := range append(append(append([]string{}, res.Added...), res.Modified...), res.Removed...) {
			changed[id] = true
		}
		for _, id := range res.Added {
			best, score := "", 0.0
			for old, text := range res.Statements {
				if changed[old] || !strings.HasPrefix(old, "intent/") {
					continue
				}
				if s := adopt.Similarity(text, res.Statements[id]); s > score || s == score && old < best {
					best, score = old, s
				}
			}
			type candidate struct {
				old, detail, text string
			}
			var cands []candidate
			if best != "" && score >= renameGuessThreshold {
				cands = append(cands, candidate{best, fmt.Sprintf("%.0f%% of the words in common", score*100), "The new request %q reads much like the approved %q. Does it replace it?"})
			}
			// Worded afresh, a request restated with a change shares few
			// words with the old one, but its rules still show it: an
			// approved condition with only its numbers changed.
			for _, old := range renumbered(head, res.Rules[id], changed) {
				if !slices.ContainsFunc(cands, func(c candidate) bool { return c.old == old }) && !paired(qs, id, old) {
					cands = append(cands, candidate{old, "a rule of the new request is a rule of the approved one with its numbers changed", "The new request %q changes a number in the approved %q. Does it replace it?"})
				}
			}
			for _, cd := range cands {
				best := cd.old
				q := Question{ID: qid("restates", id, best, translate.TextHash(res.Statements[id])), Kind: "restates", AskOwner: true,
					Requirements: []Requirement{req(id), req(best)}, Statements: []string{id},
					Detail: cd.detail}
				q.Requirements[1].Where = "current spec"
				q.Text = fmt.Sprintf(cd.text, req(id).Title, req(best).Title)
				q.Recommended, q.Why = "update_old", "a request restated with a change replaces the earlier one; kept, the spec would say both"
				q.Options = []Option{
					{Choice: "update_old", Label: "It replaces the earlier request: retire it", Then: "Stele retires the earlier request and its rules (`.stele/retired-intents.yaml`, committed with the change); run `stele change` again"},
					{Choice: "both", Label: "Both stand: they are different requirements", Then: "run `stele change` again"},
				}
				if !answered(as, q) {
					qs = append(qs, q)
				}
			}
		}
	}

	// A threshold read through a new setting that no requirement gives a
	// value is checked for every value, not the one meant: an
	// overdue rule read "now - lentAt > config.loanPeriod" beside an
	// approved due date of lentAt + 14d (a developer trial, 2026-11-03).
	// The owner says whether the request meant a fixed value.
	for _, sym := range freeSettings(res, head) {
		var stmts []string
		for _, id := range append(append([]string{}, res.Added...), res.Modified...) {
			for _, rid := range res.Rules[id] {
				if x := head.Rule(rid); x != nil && strings.Contains(english(x), sym.path) && !slices.Contains(stmts, id) {
					stmts = append(stmts, id)
				}
			}
		}
		if len(stmts) == 0 {
			continue
		}
		q := Question{ID: qid("free_setting", append([]string{sym.name}, stmts...)...), Kind: "free_setting", AskOwner: true, Statements: stmts}
		for _, id := range stmts {
			q.Requirements = append(q.Requirements, req(id))
		}
		q.Text = fmt.Sprintf("Stele read %q through a new setting, %s, that no requirement gives a value, so it checks the rules for every value of it rather than the one meant, and a clash with that value goes unseen. Does the request mean a fixed value, or one named by an approved requirement?", q.Requirements[0].Title, Words(sym.name, ""))
		q.Recommended, q.Why = "fixed", "a value the requirements fix is checked against them; a free setting is not"
		q.Options = []Option{
			{Choice: "fixed", Label: "A fixed value: say which (the owner's words)", Then: "Stele reads the requirement again with the owner's note; run `stele change` again", Note: true},
			{Choice: "setting", Label: "A setting someone configures: any value", Then: "run `stele change` again"},
		}
		if !answered(as, q) {
			qs = append(qs, q)
		}
	}

	// A request that gives Stele nothing to check is often a one-off task,
	// not something the product must keep ("write the review", "set up the
	// tracker"): approving it would record a requirement that protects
	// nothing (a developer trial, 2026-11-02). The owner says which. The
	// question says why Stele asks, what each answer means and what
	// follows, since the owner reads it as the agent relays it, without
	// Stele's terms: "found nothing to check ... requirement or one-off
	// task?" was answered "task" for a requirement the owner meant to keep
	// (the owner, 2026-11-07).
	if res.Format == "shale" {
		for _, id := range append(append([]string{}, res.Added...), res.Modified...) {
			if _, failed := res.Failed[id]; failed || len(res.Rules[id]) > 0 || !strings.HasPrefix(id, "intent/") {
				continue
			}
			q := Question{ID: qid("task", id, translate.TextHash(res.Statements[id])), Kind: "task", AskOwner: true,
				Requirements: []Requirement{req(id)}, Statements: []string{id}}
			q.Text = fmt.Sprintf("The request %q states nothing Stele can check in the code: no behaviour the product keeps, such as what it does, never does or defaults to. Was it a one-off job, done once the work is done (a review, a write-up, a trial)? Or is it a requirement the product must keep from now on, which Stele records in the spec without being able to check it?", req(id).Title)
			q.Options = []Option{
				{Choice: "task", Label: "A one-off job: done when the work is done, never approved as a requirement", Then: "Stele sets the request aside in .stele/retired-intents.yaml and it leaves the change; run `stele change` again"},
				{Choice: "requirement", Label: "A requirement the product keeps from now on: Stele records it in the spec without checking it", Then: "it goes to the approval as a requirement Stele records but does not check; run `stele change` again"},
			}
			if !answered(as, q) {
				qs = append(qs, q)
			}
		}
	}

	// The approval, once nothing else is open.
	// A change that only retires rules needs the approval too: it is what
	// applies the retirement, and without it the registry kept the removed
	// requirement's rules live (a code review, 2026-11-03).
	if g != nil && len(g.Errors) == 0 && (g.Accepted || g.OnlyUndecided()) && len(qs) == 0 && (len(g.Unapproved) > 0 || len(res.Retired) > 0) && len(res.Failed) == 0 {
		var checked []string
		for id := range inChange {
			if len(res.Rules[id]) > 0 {
				checked = append(checked, id)
			}
		}
		sort.Strings(checked)
		q := Question{ID: qid("approve", append(slices.Clone(g.Unapproved), res.Retired...)...), Kind: "approve", AskOwner: true}
		flagged, partial, clauses := 0, 0, 0
		for _, id := range checked {
			r := req(id)
			for _, rid := range res.Rules[id] {
				if x := head.Rule(rid); x != nil {
					r.Reading = append(r.Reading, english(x))
				}
			}
			r.Findings, r.NotChecked, _ = SplitFindings(res.Findings[id])
			if len(r.Findings) > 0 {
				flagged++
			}
			if len(r.NotChecked) > 0 {
				partial++
				clauses += len(r.NotChecked)
			}
			q.Requirements = append(q.Requirements, r)
		}
		switch {
		case len(checked) == 0 && len(res.Retired) == 0:
			q.Text = "Stele has no rule to check for this change, so approving it records its requirements unchecked"
		case len(checked) == 0:
			q.Text = fmt.Sprintf("This change retires %d approved rule(s) (%s) and adds none", len(res.Retired), strings.Join(res.Retired, ", "))
		case !res.first:
			q.Text = fmt.Sprintf("%d requirement(s) in this change are checked and agree with the approved spec", len(checked))
			if len(checked) == 1 {
				q.Text = "The 1 requirement in this change is checked and agrees with the approved spec"
			}
		default:
			// The first change: there is nothing approved to agree with yet.
			q.Text = fmt.Sprintf("%d requirement(s) in this change are checked and agree with each other; nothing is approved yet, so these are the first", len(checked))
			if len(checked) == 1 {
				q.Text = "The 1 requirement in this change is checked; nothing is approved yet, so it is the first"
			}
		}
		if len(checked) > 0 && len(res.Retired) > 0 {
			q.Text += fmt.Sprintf("; it retires %d approved rule(s) (%s)", len(res.Retired), strings.Join(res.Retired, ", "))
		}
		if partial > 0 {
			q.Text += fmt.Sprintf("; %d checked in part: the report lists the %d clause(s) no rule checks", partial, clauses)
		}
		if flagged > 0 {
			q.Text += fmt.Sprintf("; Stele's reading of %d says more than the text, or blocks a step (see findings)", flagged)
			q.Recommended, q.Why = "show", "look at the flagged readings before approving them"
		}
		if g.OnlyUndecided() {
			// One step the solver cannot settle no longer holds up every
			// later change that touches it (a developer trial, 2026-11-03).
			q.Text += fmt.Sprintf("; the solver could not decide what %s must do, so the rules there are approved unchecked: a contradiction in that step would not be found", strings.Join(g.Check.Undecided, ", "))
			q.Recommended, q.Why = "show", "the rules of the step Stele could not decide are not checked"
		}
		if n := len(res.Unchecked); n > 0 {
			var titles []string
			for _, id := range sortedKeys(res.Unchecked) {
				titles = append(titles, fmt.Sprintf("%q", req(id).Title))
			}
			q.Text += fmt.Sprintf("; %d could not be checked (%s)", n, strings.Join(titles, ", "))
		}
		// The count of new terms was Stele's bookkeeping, not the owner's
		// decision; agents relayed it as a list of internal names (a live
		// trial, 2026-11-08). The report still lists them for the agent.
		if k := keeps(as); len(k) > 0 {
			var said []string
			for _, op := range slices.Sorted(maps.Keys(k)) {
				var vs []string
				for _, w := range k[op] {
					vs = append(vs, strings.Trim(Words(w, meaningOf(head, w)), `"`))
				}
				said = append(said, strings.Trim(Words(op, ""), `"`)+" keeps "+strings.Join(vs, ", "))
			}
			q.Text += ". It corrects what approved steps change, as the agent answered: " + strings.Join(said, "; ")
		}
		if n := len(res.NewLessons); n > 0 {
			q.Text += fmt.Sprintf(". Your answers also teach Stele %d lesson(s) about this project, saved in the registry with the change: %s", n, strings.Join(res.NewLessons, "; "))
		}
		q.Text += ". Approve this change?"
		q.Detail = strings.Join(append(slices.Clone(g.Unapproved), res.Retired...), "\n")
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

// onlyKept reports whether a writes-changed detail ("A.x, A.y -> A.x")
// only drops values the answers say the step keeps: the agent decided that
// from the step's rules, and the approval names it.
func onlyKept(detail string, kept []string) bool {
	before, after, ok := strings.Cut(detail, " -> ")
	if !ok || len(kept) == 0 {
		return false
	}
	list := func(s string) []string {
		var out []string
		for _, x := range strings.Split(s, ", ") {
			// The gate names writes by QName (Job$result.code, Tool.lentTo?),
			// answers by the path the step writes (Job.result.code).
			if x = strings.TrimSuffix(strings.ReplaceAll(strings.TrimSpace(x), "$", "."), "?"); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	b, a := list(before), list(after)
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	for _, x := range b {
		if !slices.Contains(a, x) && !slices.Contains(kept, x) {
			return false
		}
	}
	return true
}

// keptAny reports whether an answer says some step keeps the value.
func keptAny(kept map[string][]string, symbol string) bool {
	for _, ws := range kept {
		if slices.Contains(ws, symbol) {
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
		if a.Kind == "creator" && a.Choice != "none" && a.Choice != "" && !slices.Contains(out[a.Existing], a.Choice) {
			out[a.Existing] = append(out[a.Existing], a.Choice)
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
			// Said plainly: an agent offered it as keeping both requests,
			// and the owner nearly retired six approved rules unawares (a
			// developer trial, 2026-11-03).
			os[i].Label = "One is an exception to the other: retire the earlier request and restate both as one"
			os[i].Then = "Stele retires the earlier request with all its rules, not only the clause in conflict; declare this request again stating the whole rule with its exception (such as \"at most 5, or up to 10 during the program\") and every other clause of the earlier request, with `shale intent \"<title>\" --body \"...\"`, show the owner the wording, then run `stele change` again"
		}
	}
}

// Reading is how Stele reads a rule, in one line, as the approval question
// shows it.
func Reading(x *ir.Rule) string { return english(x) }

// factKey is what a world fact says, as the parser prints it, so a fact
// proposed again under another ID, or worded with other spacing, is the
// same fact.
func factKey(on, when, assert string) string {
	norm := func(src string) string {
		if e, err := ir.ParseExpr(src); err == nil && src != "" {
			return e.String()
		}
		return strings.TrimSpace(src)
	}
	return on + "\x00" + norm(when) + "\x00" + norm(assert)
}

func ruleFactKey(x *ir.Rule) string {
	str := func(e ir.Expr) string {
		if e == nil {
			return ""
		}
		return e.String()
	}
	return factKey(x.On, str(x.When), str(x.Assert))
}

// rejectedFact reports whether the owner said this world fact is wrong:
// by its ID, or by what it says.
func rejectedFact(as []Answer, id, key string) bool {
	return slices.ContainsFunc(as, func(a Answer) bool {
		return a.Kind == "conflict" && a.Choice == "wrong_fact" && (a.Link == id || slices.Contains(a.Links, id) || slices.Contains(a.Facts, key))
	})
}

func candFactKey(c translate.Candidate) string { return factKey(c.On, c.When, c.Assert) }

// numberLiteral matches a number in a rule's text, with a duration's unit.
var numberLiteral = regexp.MustCompile(`\b\d+(\.\d+)?[a-z]*\b`)

// renumbered lists the approved requests, unchanged in this change, with a
// rule that a rule of the new one restates with its numbers changed: on the
// same step, of the same kind and effect, with a condition or assertion
// that contains the approved one's once numbers are set aside, but not as
// written. "count(...) >= 2" restates "count(...) >= 3" (a developer
// trial, 2026-11-03).
func renumbered(head *ir.Registry, rules []string, changed map[string]bool) []string {
	if head == nil {
		return nil
	}
	text := func(e ir.Expr) string {
		if e == nil {
			return ""
		}
		return e.String()
	}
	skeleton := func(s string) string { return numberLiteral.ReplaceAllString(s, "#") }
	found := map[string]bool{}
	for _, rid := range rules {
		n := head.Rule(rid)
		if n == nil {
			continue
		}
		for _, o := range head.Rules {
			if o.StatementID == "" || changed[o.StatementID] || !strings.HasPrefix(o.StatementID, "intent/") || found[o.StatementID] {
				continue
			}
			if o.Kind != n.Kind || o.Effect != n.Effect || o.Operation != n.Operation || o.On != n.On {
				continue
			}
			for _, pair := range [][2]string{{text(o.When), text(n.When)}, {text(o.Assert), text(n.Assert)}} {
				was, now := pair[0], pair[1]
				if !numberLiteral.MatchString(was) || strings.Contains(now, was) {
					continue
				}
				if strings.Contains(skeleton(now), skeleton(was)) {
					found[o.StatementID] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(found))
}

// paired reports whether a question already asks about requests a and b
// together.
func paired(qs []Question, a, b string) bool {
	for _, q := range qs {
		var ids []string
		for _, r := range q.Requirements {
			ids = append(ids, r.ID)
		}
		ids = append(ids, q.Statements...)
		if slices.Contains(ids, a) && slices.Contains(ids, b) {
			return true
		}
	}
	return false
}

// setting is a new attribute a rule reads as a threshold.
type setting struct{ name, path string }

// freeSettings are the attributes this change adds that the world sets,
// that hold a number, duration or time, on an entity no step writes and
// that is not the request, the clock or a result, and that no state
// invariant gives a value.
func freeSettings(res *Result, head *ir.Registry) []setting {
	var out []setting
	for _, sym := range res.NewTerms {
		ent, name, ok := strings.Cut(sym, ".")
		if !ok || head == nil || head.Entities[ent] == nil {
			continue
		}
		a := head.Entities[ent].Attrs[name]
		if a == nil || !a.WorldSet || a.Kind != ir.Int && a.Kind != ir.Decimal && a.Kind != ir.Duration && a.Kind != ir.Time {
			continue
		}
		switch ent {
		case "Request", "Actor", "Clock", "Result":
			continue
		}
		written := false
		for _, op := range head.Operations {
			written = written || slices.ContainsFunc(op.Writes, func(w *ir.Attr) bool { return w.Entity != nil && w.Entity.Name == ent })
		}
		if written {
			continue
		}
		path := strings.ToLower(ent[:1]) + ent[1:] + "." + name
		fixed := false
		for _, x := range head.Rules {
			if x.Kind == ir.Invariant && x.On == "" && x.Status != "retired" && strings.Contains(english(x), path) {
				fixed = true
			}
		}
		if !fixed {
			out = append(out, setting{sym, path})
		}
	}
	return out
}
