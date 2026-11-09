package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/provasign/stele/internal/config"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/change"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
)

// cmdChangeAnswer records the owner's answer to one question of a change
// check (D-058). Approving applies the change's rules to the registry and
// records the approval; the other answers take effect on the next check.
func cmdChangeAnswer(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("change answer", flag.ExitOnError)
	outDir := fs.String("out", "", "the check's work directory (default .stele/change/<id>)")
	note := fs.String("note", "", "the owner's words, for an answer that needs them")
	quoted := fs.Bool("quoted", false, "the note is the owner's own words, as they said them: only such a note teaches Stele a lesson")
	by := fs.String("by", "", "who answers (default: git user.name)")
	undo := fs.Bool("undo", false, "remove the recorded answer to the question; the next check asks it again")
	usage := "usage: stele change answer <change-id> <question-id> (<choice> | --undo) [--note TEXT [--quoted]] [--by NAME] [--out DIR]\n"
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	id, qid, choice := args[0], args[1], ""
	rest := args[2:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		choice, rest = rest[0], rest[1:]
	}
	_ = fs.Parse(rest)
	if *outDir == "" {
		*outDir = filepath.Join(".stele", "change", id)
	}
	if *undo {
		ok, err := change.RemoveAnswer(*outDir, qid)
		if err != nil {
			return failf("change answer", err)
		}
		if !ok {
			return failf("change answer", fmt.Errorf("no recorded answer to %s", qid))
		}
		fmt.Fprintf(out, "Removed the answer to %s. Next: run `stele change` again.\n", qid)
		return 0
	}
	if choice == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if *by == "" {
		*by = owner()
	}
	src, err := os.ReadFile(filepath.Join(*outDir, "result.json"))
	if err != nil {
		return failf("change answer", fmt.Errorf("no check for %s: run `stele change openspec <repo> %s` first (%w)", id, id, err))
	}
	var res change.Result
	if err := json.Unmarshal(src, &res); err != nil {
		return failf("change answer", err)
	}
	i := slices.IndexFunc(res.Questions, func(q change.Question) bool { return q.ID == qid })
	if i < 0 {
		return failf("change answer", fmt.Errorf("the last check of %s has no question %s; run `stele change` again for the current questions", id, qid))
	}
	q := res.Questions[i]
	k := slices.IndexFunc(q.Options, func(o change.Option) bool { return o.Choice == choice })
	if k < 0 {
		var cs []string
		for _, o := range q.Options {
			cs = append(cs, o.Choice)
		}
		return failf("change answer", fmt.Errorf("question %s takes one of: %s", qid, strings.Join(cs, ", ")))
	}
	opt := q.Options[k]
	a := change.Answer{Question: q.ID, Kind: q.Kind, Choice: choice, Note: *note, Quoted: *quoted && strings.TrimSpace(*note) != "", By: *by, Term: q.Term, Existing: q.Existing, Link: q.Link, Links: q.Links, Facts: q.Facts, Statements: map[string]string{}}
	if opt.Note && strings.TrimSpace(a.Note) == "" {
		return failf("change answer", fmt.Errorf("%q needs the owner's words: pass --note", opt.Label))
	}
	if choice == "impossible" {
		// The situation the owner rules out, plus the owner's words.
		a.Said = strings.TrimSpace(a.Note)
		a.Note = strings.TrimSpace(q.Example + ". " + a.Note)
	}
	if q.Kind == "self_conflict" && a.Note == "" {
		a.Note = strings.ReplaceAll(q.Detail, "\n", "; ")
		if q.Example != "" {
			a.Note += "; for example when " + q.Example
		}
	}
	for _, r := range q.Requirements {
		if slices.Contains(q.Statements, r.ID) {
			a.Statements[r.ID] = translate.TextHash(r.Text)
		} else if choice == "precedence" {
			a.Over = append(a.Over, r.ID)
		}
	}
	if *by == "agent" && choice == "misread" {
		as, err := change.LoadAnswers(*outDir)
		if err != nil {
			return failf("change answer", err)
		}
		for id, h := range a.Statements {
			if change.AgentReread(as, id, h) {
				return failf("change answer", fmt.Errorf(agentRereadDone, id))
			}
		}
	}
	switch {
	case q.Kind == "approve" && choice == "approve":
		if err := approveChange(&res, *by, out); err != nil {
			return failf("change answer", err)
		}
	case choice == "show":
		fmt.Fprintf(out, "Show the owner the requirements with Stele's reading (%s, \"Rules to approve\"), then ask again.\n", filepath.Join(*outDir, "report.md"))
		return 0
	case choice == "not_yet":
		fmt.Fprintln(out, "Nothing recorded.")
		return 0
	}
	if err := change.SaveAnswer(*outDir, a); err != nil {
		return failf("change answer", err)
	}
	if res.Format == "shale" && q.Kind == "task" && choice == "task" {
		var set []importer.RetiredIntent
		for _, id := range q.Statements {
			set = append(set, importer.RetiredIntent{ID: id, By: *by, At: time.Now().UTC().Format("2006-01-02"), Why: "not a requirement"})
		}
		if err := importer.RetireIntents(res.Repo, set); err != nil {
			return failf("change answer", err)
		}
		if err := importer.KeepIntents(res.Repo, nil, q.Statements); err != nil {
			return failf("change answer", err)
		}
	}
	if res.Format == "shale" && (choice == "update_old" || choice == "exception") {
		// Shale's evidence is append-only: the older requests are retired
		// in Stele's own file, and the next check removes their rules. For
		// an exception, the new request is declared again as the whole rule.
		var old []importer.RetiredIntent
		for _, r := range q.Requirements {
			if r.Where == "current spec" && strings.HasPrefix(r.ID, "intent/") {
				old = append(old, importer.RetiredIntent{ID: r.ID, By: *by, At: time.Now().UTC().Format("2006-01-02"), For: strings.Join(q.Statements, ", ")})
			}
		}
		if err := importer.RetireIntents(res.Repo, old); err != nil {
			return failf("change answer", err)
		}
	}
	if *by != "agent" {
		// The owner's decisions travel with the change, on its PR card.
		if err := recordDecision(res.Repo, id, q, opt.Label, *by); err != nil {
			fmt.Fprintf(os.Stderr, "stele change answer: could not update the PR card: %v\n", err)
		}
	}
	if q.Kind == "approve" && choice == "approve" {
		return 0 // approveChange already said what it wrote
	}
	then := opt.Then
	if id == adoptChange {
		then = forAdoption(then)
	}
	fmt.Fprintf(out, "Recorded %s: %s. Next: %s.\n", q.ID, opt.Label, then)
	return 0
}

// approveChange writes the checked change into the registry and records the
// owner's approval of every rule it adds or edits, if the check is current.
func approveChange(res *change.Result, by string, out io.Writer) error {
	g := res.Gate
	if g == nil || len(g.Errors) > 0 || !g.Accepted && !g.OnlyUndecided() {
		return fmt.Errorf("the check found problems; it cannot be approved")
	}
	now, err := change.Fingerprint(res.Watched...)
	if err != nil {
		return err
	}
	if now != res.Fingerprint {
		return fmt.Errorf("the change or the registry changed since the check; run `stele change` again")
	}
	if err := copyDir(res.Head, res.Registry); err != nil {
		return err
	}
	reg, err := ir.LoadDir(res.Registry)
	if err != nil {
		return err
	}
	if err := adopt.ApproveRules(reg, g.Unapproved, by); err != nil {
		return err
	}
	if g.OnlyUndecided() {
		if err := recordUnchecked(res.Registry, reg, g.Check.Undecided, by); err != nil {
			return err
		}
	}
	if res.Change == adoptChange {
		// The adopted specs are where saved-spec sync starts from.
		if cfg, err := config.Find(res.Repo); err == nil && cfg != nil && cfg.Specs != nil {
			if stmts, err := importSpecs(res.Repo, cfg.Specs); err == nil {
				if err := writeSnapshot(cfg.SnapshotFile(), stmts, cfg.Specs.Format); err != nil {
					return err
				}
			}
		}
	}
	fmt.Fprintf(out, "Applied %s to %s and approved %d rule(s) as %s at %s. Commit the registry with the change.\n", res.Change, res.Registry, len(g.Unapproved), by, time.Now().Format("2006-01-02"))
	return nil
}

// owner names who answers: git's user.name, else the login name.
func owner() string {
	if b, err := exec.Command("git", "config", "user.name").Output(); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	return os.Getenv("USER")
}

// agentRereadDone refuses the agent a second reread of one requirement in
// a change (change.AgentReread).
const agentRereadDone = "Stele already read %s again once at the agent's note in this change, and does not again: the clause stays as Stele reads it, and the owner sees it among the findings at the approval. If the reading is wrong in a way that matters, ask the owner"

// cmdChangeReread records the owner's note that Stele misread one
// requirement of the change, outside any question: the next check reads
// that requirement again with the note.
func cmdChangeReread(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("change reread", flag.ExitOnError)
	outDir := fs.String("out", "", "the check's work directory (default .stele/change/<id>)")
	note := fs.String("note", "", "what Stele got wrong, in the owner's words (required)")
	quoted := fs.Bool("quoted", false, "the note is the owner's own words, as they said them: only such a note teaches Stele a lesson")
	by := fs.String("by", "", "who says so (default: git user.name)")
	if len(args) < 2 {
		fmt.Fprint(os.Stderr, "usage: stele change reread <change-id> <requirement> --note TEXT\n")
		return 2
	}
	id, which := args[0], args[1]
	_ = fs.Parse(args[2:])
	if strings.TrimSpace(*note) == "" {
		return failf("change reread", fmt.Errorf("say what Stele got wrong with --note"))
	}
	if *outDir == "" {
		*outDir = filepath.Join(".stele", "change", id)
	}
	if *by == "" {
		*by = owner()
	}
	src, err := os.ReadFile(filepath.Join(*outDir, "result.json"))
	if err != nil {
		return failf("change reread", fmt.Errorf("no check for %s: run `stele change` first (%w)", id, err))
	}
	var res struct {
		Added, Modified []string
		Texts           map[string]string `json:"texts"`
	}
	if err := json.Unmarshal(src, &res); err != nil {
		return failf("change reread", err)
	}
	// The requirement by its ID, the part after the last slash (FR-012),
	// or the start of its title.
	var found []string
	for _, sid := range append(append([]string{}, res.Added...), res.Modified...) {
		tail := sid[strings.LastIndex(sid, "/")+1:]
		if sid == which || tail == which || strings.HasPrefix(strings.ToLower(tail), strings.ToLower(which)) {
			found = append(found, sid)
		}
	}
	if len(found) != 1 {
		return failf("change reread", fmt.Errorf("%q matches %d requirements this change adds or edits (%s); name one by its ID", which, len(found), strings.Join(found, ", ")))
	}
	text := res.Texts[found[0]]
	if text == "" {
		return failf("change reread", fmt.Errorf("no text recorded for %s; run `stele change` again", found[0]))
	}
	a := change.Answer{Question: "reread-" + found[0], Kind: "reread", Choice: "reread", Note: *note, Quoted: *quoted, By: *by,
		Statements: map[string]string{found[0]: translate.TextHash(text)}}
	if *by == "agent" {
		as, err := change.LoadAnswers(*outDir)
		if err != nil {
			return failf("change reread", err)
		}
		if change.AgentReread(as, found[0], a.Statements[found[0]]) {
			return failf("change reread", fmt.Errorf(agentRereadDone, found[0]))
		}
	}
	if err := change.SaveAnswer(*outDir, a); err != nil {
		return failf("change reread", err)
	}
	fmt.Fprintf(out, "Recorded: Stele reads %s again with the note. Next: run `stele change` again.\n", found[0])
	return 0
}

// recordUnchecked writes, for each undecided step not yet recorded, that
// the owner approved it knowing it stays unchecked, so CI's gate accepts
// what the owner accepted here.
func recordUnchecked(dir string, reg *ir.Registry, steps []string, by string) error {
	var b strings.Builder
	for _, step := range steps {
		if reg.Unchecked[step] != nil {
			continue
		}
		fmt.Fprintf(&b, "---\n# The owner approved rules on this step though the solver could not decide it: it is not checked.\nunchecked: %s\nby: %s\nat: %q\n", step, strconv.Quote(by), time.Now().UTC().Format("2006-01-02"))
	}
	if b.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "unchecked.yaml"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
