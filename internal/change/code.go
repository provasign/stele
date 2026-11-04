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
	for i := range res.Questions {
		q := &res.Questions[i]
		if q.Kind != "conflict" || !q.AskOwner || len(q.stmts) < 2 {
			continue
		}
		h := sha256.New()
		fmt.Fprintf(h, "%s\x00%s\x00%s", q.ID, state, q.Example)
		var stmts []translate.Statement
		for _, r := range q.Requirements {
			fmt.Fprintf(h, "\x00%s\x00%s", r.ID, r.Text)
			stmts = append(stmts, translate.Statement{ID: r.ID, Prose: r.Text, Source: r.Source})
		}
		key := hex.EncodeToString(h.Sum(nil))[:16]
		v, ok := cache[key]
		if !ok {
			situation := q.Example
			if situation == "" {
				situation = q.Text
			}
			if opt.Log != nil {
				fmt.Fprintf(opt.Log, "code: what the code does for %s\n", strings.Join(slices.Compact(slices.Sorted(slices.Values(q.stmts))), ", "))
			}
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
			ids := slices.Compact(slices.Sorted(slices.Values(q.stmts)))
			vs, usage := adjudicate.Decide(ctx, opt.Clients[0], stmts, []adjudicate.Candidate{{StatementIDs: ids, Situation: situation}},
				adjudicate.Options{Repo: opt.Repo, Evidence: evidence, Ranges: true, Timeout: opt.RequestTimeout}, nil)
			res.Usage.Add(usage)
			v = vs[0]
			if !strings.HasPrefix(v.Reason, "model call failed") && !strings.HasPrefix(v.Reason, "code search failed") {
				cache[key] = v
			}
		}
		applyCode(q, v)
	}
	if src, err := json.MarshalIndent(cache, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(opt.Out, codeFile), src, 0o644)
	}
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
