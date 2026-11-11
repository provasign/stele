package change

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/provasign/stele/internal/adjudicate"
	"github.com/provasign/stele/internal/codegraph"
	"github.com/provasign/stele/internal/translate"
)

// Code is what the project's code does in a conflict's situation (D-062):
// which requirements it follows, with quotes checked against the files.
type Code struct {
	// Says is one sentence for the owner, in the spec's words.
	Says    string            `json:"says"`
	Outcome string            `json:"outcome"` // follows | exception | not_implemented | compatible | unclear
	Follows []string          `json:"follows,omitempty"`
	Against []string          `json:"against,omitempty"`
	Cites   []adjudicate.Cite `json:"cites,omitempty"`
}

// codeFile caches verdicts by question, requirement texts and code state.
const codeFile = "code.json"

// codeEvidence asks what the code does for each conflict the owner is asked
// about, and recommends the answer the code already follows. A verdict with
// no quote that checks out against the files recommends nothing.
func codeEvidence(ctx context.Context, opt Options, res *Result) {
	if len(opt.Clients) == 0 || opt.Repo == "" {
		return
	}
	var sources []string
	for _, s := range res.Sources {
		sources = append(sources, s)
	}
	exclude := adjudicate.SpecDirs(sources)
	for _, d := range []string{opt.Registry, opt.Out} {
		if rel, err := relTo(opt.Repo, d); err == nil {
			exclude = append(exclude, rel)
		}
	}
	state := codeState(opt.Repo, exclude)
	// Range requests are the measured default (2026-10-27); definitions only
	// come from the code graph, as whole bodies.
	evidence := adjudicate.EvidenceOptions{Exclude: exclude, NoDefinitions: true}
	var graph *codegraph.Graph
	defer func() {
		if graph != nil {
			graph.Close()
		}
	}()
	cache := map[string]adjudicate.Verdict{}
	if src, err := os.ReadFile(filepath.Join(opt.Out, codeFile)); err == nil {
		_ = json.Unmarshal(src, &cache)
	}
	answers, _ := LoadAnswers(opt.Out)
	rereading := map[string]bool{}
	var wait []string
	overBudget := false
	fresh := map[string]adjudicate.Verdict{} // verdicts of this run, also those not kept
	// The code is read for every question at once, four at a time: read
	// one after another, four questions took a minute and a half of an
	// eleven-minute adoption (a live session, 2026-11-10).
	type job struct {
		key string
		q   *Question
	}
	var jobs []job
	var cands []adjudicate.Candidate
	var all []translate.Statement
	keyOf := map[string]string{} // question -> cache key
	for i := range res.Questions {
		q := &res.Questions[i]
		if q.Kind != "conflict" || !q.AskOwner || len(q.stmts) < 2 {
			continue
		}
		h := sha256.New()
		fmt.Fprintf(h, "%s\x00%s\x00%s", q.ID, state, q.Example)
		for _, r := range q.Requirements {
			fmt.Fprintf(h, "\x00%s\x00%s", r.ID, r.Text)
		}
		key := hex.EncodeToString(h.Sum(nil))[:16]
		keyOf[q.ID] = key
		if _, ok := cache[key]; ok {
			continue
		}
		if opt.MaxCost > 0 && res.Usage.CostUSD >= opt.MaxCost {
			// The owner's limit covers the whole check, not only the
			// translation: an adoption capped at $3 spent $3.66 with these
			// calls on top (2026-11-10). The questions are asked without
			// what the code does; the next check, with a budget, adds it.
			if !overBudget && opt.Log != nil {
				fmt.Fprintf(opt.Log, "code: the cost limit ($%.2f) is reached; the remaining conflicts are asked without what the code does\n", opt.MaxCost)
			}
			overBudget = true
			continue
		}
		situation := q.Example
		if situation == "" {
			situation = q.Text
		}
		for _, r := range q.Requirements {
			if !slices.ContainsFunc(all, func(s translate.Statement) bool { return s.ID == r.ID }) {
				all = append(all, translate.Statement{ID: r.ID, Prose: r.Text, Source: r.Source})
			}
		}
		ids := slices.Compact(slices.Sorted(slices.Values(q.stmts)))
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, "code: what the code does for %s\n", strings.Join(ids, ", "))
		}
		jobs = append(jobs, job{key, q})
		cands = append(cands, adjudicate.Candidate{StatementIDs: ids, Situation: situation})
	}
	if len(jobs) > 0 {
		if graph == nil && opt.Graph {
			g, err := codegraph.Open(ctx, opt.Repo)
			if err != nil && opt.Log != nil {
				fmt.Fprintf(opt.Log, "code: no code graph (%v); searching text only\n", err)
			}
			if graph = g; g != nil {
				evidence.Define, evidence.NoDefinitions = g.Define, false
			}
			opt.Graph = g != nil
		}
		vs, usage := adjudicate.Decide(ctx, opt.Clients[0], all, cands,
			adjudicate.Options{Repo: opt.Repo, Evidence: evidence, Ranges: true, Timeout: opt.RequestTimeout, Parallel: 4}, nil)
		res.Usage.Add(usage)
		for k, v := range vs {
			if !strings.HasPrefix(v.Reason, "model call failed") && !strings.HasPrefix(v.Reason, "code search failed") {
				cache[jobs[k].key] = v
			}
			fresh[jobs[k].key] = v
		}
	}
	for i := range res.Questions {
		q := &res.Questions[i]
		if q.Kind != "conflict" || !q.AskOwner || len(q.stmts) < 2 {
			continue
		}
		v, ok := cache[keyOf[q.ID]]
		if !ok {
			if v, ok = fresh[keyOf[q.ID]]; !ok {
				continue
			}
		}
		applyCode(q, v)
		if rereadFirst(q, answers) {
			// The agent has a requirement read again once: a second
			// question that turns on the same requirement waits for that
			// reading, and comes back if the clash is still there. Asked
			// in the same round, its answer was refused (2026-11-10).
			for _, id := range q.Statements {
				if rereading[id] {
					wait = append(wait, q.ID)
				}
			}
			for _, id := range q.Statements {
				rereading[id] = true
			}
		}
	}
	res.Questions = slices.DeleteFunc(res.Questions, func(q Question) bool { return slices.Contains(wait, q.ID) })
	if src, err := json.MarshalIndent(cache, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(opt.Out, codeFile), src, 0o644)
	}
}

// rereadFirst makes a conflict the agent's to answer when the code does what
// both requirements say in the clash's situation: the requirements then
// most likely agree and Stele read one of them too widely. Of the 12
// conflicts a first adoption asked its owner (179 requirements,
// 2026-11-10), the code showed 4 to be no conflict, each asked "Which is
// right?" with nothing recommended. The agent has Stele read the
// requirements again with what the code does as the note; it gets one such
// reread of a requirement (AgentReread), so a clash that comes back is the
// owner's, with what the code does beside it.
func rereadFirst(q *Question, as []Answer) bool {
	if q.Code == nil || q.Code.Outcome != "compatible" || len(q.Statements) == 0 ||
		!slices.ContainsFunc(q.Options, func(o Option) bool { return o.Choice == "misread" }) {
		return false
	}
	if slices.ContainsFunc(as, func(a Answer) bool { return a.Question == q.ID && a.By != "agent" }) {
		// The owner has answered this very question and it still clashes:
		// it stays theirs. Handed to the agent, the agent's answer
		// replaced the owner's precedence, and the question came back to
		// the owner a third time (a first adoption, 2026-11-10).
		return false
	}
	for _, r := range q.Requirements {
		if slices.Contains(q.Statements, r.ID) && AgentReread(as, r.ID, translate.TextHash(r.Text)) {
			if q.Recommended == "" {
				// Read again and still clashing, where the code does both:
				// the owner got the question with nothing recommended and
				// an example made of Stele's own reading (a trial,
				// 2026-11-10).
				recommend(q, "later", "Stele read these again and they still clash, while the code does what both say: this is most likely Stele's reading, not the specs. Deciding later keeps both requirements and records the clash as open")
			}
			return false
		}
	}
	q.AskOwner = false
	recommend(q, "misread", q.Code.Says+" So this is most likely Stele's reading, not a contradiction in the spec: answer `misread` with --by agent, and as the note say what the requirement's own words mean that Stele's reading missed (the code shows which reading is meant; do not put what the code does into the requirement). Stele reads the requirements again, and asks the owner only if they still clash.")
	return true
}

// applyCode puts a verdict on its question and recommends what the code
// follows: an exception the code already makes, or changing the
// requirement the code contradicts.
func applyCode(q *Question, v adjudicate.Verdict) {
	var cites []adjudicate.Cite
	for _, c := range v.Cites {
		if c.Valid {
			cites = append(cites, c)
		}
	}
	title := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if k := slices.IndexFunc(q.Requirements, func(r Requirement) bool { return r.ID == id }); k >= 0 {
				out = append(out, q.Requirements[k].Title)
			}
		}
		return out
	}
	where := func() string {
		if len(cites) == 0 {
			return ""
		}
		return fmt.Sprintf(" (%s:%d)", cites[0].File, cites[0].Line)
	}
	c := &Code{Follows: title(v.Follows), Cites: cites}
	switch {
	case v.Kind == "overbroad" && len(cites) > 0:
		c.Outcome, c.Against = "exception", title(v.Overbroad)
		c.Says = "The code already makes this exception: " + v.Reason + where()
		recommend(q, "exception", c.Says)
		// Where one requirement can take precedence with no edit to the
		// spec, recommend that, the way the code has it: the requirement
		// the code finds too broad gives way.
		for _, o := range q.Options {
			if len(o.Prevails) == 0 || len(v.Overbroad) == 0 {
				continue
			}
			if !slices.ContainsFunc(v.Overbroad, func(id string) bool { return !slices.Contains(o.Over, id) || slices.Contains(o.Prevails, id) }) {
				recommend(q, o.Choice, c.Says)
				break
			}
		}
	case v.Kind == "superseded" && len(cites) > 0:
		c.Outcome, c.Against = "follows", title(v.Stale)
		c.Says = "The code follows " + quoteAll(c.Follows) + ", not " + quoteAll(c.Against) + ": " + v.Reason + where()
		mine, older := 0, 0
		for _, id := range v.Stale {
			k := slices.IndexFunc(q.Requirements, func(r Requirement) bool { return r.ID == id })
			if k >= 0 && q.Requirements[k].Where == "current spec" {
				older++
			} else if k >= 0 {
				mine++
			}
		}
		switch {
		case mine > 0 && older == 0:
			recommend(q, "change", c.Says)
		case older > 0 && mine == 0:
			recommend(q, "update_old", c.Says)
		}
	case v.Kind == "not_implemented":
		c.Outcome, c.Says = "not_implemented", "The code does not implement this yet: "+v.Reason
	case v.Kind == "compatible" || v.Kind == "resolved_in_text":
		c.Outcome, c.Says = "compatible", "In the code the two do not clash: "+v.Reason+where()
		if strings.HasPrefix(q.Recommended, "prevails") {
			// The owner hears that the code keeps the two apart and is
			// offered an exception: say that the answer records just that.
			q.Why += "; the code already keeps the two apart, and this answer records that order with no edit to the spec"
		}
	default:
		return // nothing the owner can use
	}
	q.Code = c
}

// recommend offers choice first, if the question has it.
func recommend(q *Question, choice, why string) {
	k := slices.IndexFunc(q.Options, func(o Option) bool { return o.Choice == choice })
	if k < 0 {
		return
	}
	q.Recommended, q.Why = choice, why
	q.Options = append([]Option{q.Options[k]}, slices.Delete(slices.Clone(q.Options), k, k+1)...)
}

func quoteAll(titles []string) string {
	qs := make([]string, len(titles))
	for i, t := range titles {
		qs[i] = fmt.Sprintf("%q", t)
	}
	return strings.Join(qs, " and ")
}

// codeState identifies the code a verdict was made on: the commit and any
// uncommitted edits outside the specs and Stele's files.
func codeState(repo string, exclude []string) string {
	head, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	args := []string{"-C", repo, "diff", "HEAD", "--", "."}
	for _, x := range exclude {
		args = append(args, ":(exclude)"+strings.TrimSuffix(x, "/"))
	}
	diff, _ := exec.Command("git", args...).Output()
	sum := sha256.Sum256(diff)
	return strings.TrimSpace(string(head)) + ":" + hex.EncodeToString(sum[:8])
}

func relTo(repo, dir string) (string, error) {
	r, err1 := filepath.Abs(repo)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("path")
	}
	rel, err := filepath.Rel(r, d)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%s is not inside %s", dir, repo)
	}
	return filepath.ToSlash(rel), nil
}
