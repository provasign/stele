package change

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/provasign/stele/internal/adjudicate"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

func TestRemovedAttributesLeaveWrites(t *testing.T) {
	p := vocab.Proposal{
		Entities: []vocab.Entity{{Name: "Pager", Attributes: []vocab.Attribute{
			{Name: "spawned", Type: "bool"}, {Name: "spawnedEarly", Type: "bool"}, {Name: "started", Type: "bool"}}}},
		Operations: []vocab.Operation{{Name: "Pager.launch", Default: "allow",
			Writes: []string{"Pager.spawned", "Pager.spawnedEarly", "Pager.started"}}},
	}
	rules := []translate.Candidate{{Outcome: "rule", On: "Pager.launch", Assert: "self.spawned and self.started"}}
	got := prune(p, rules).Operations[0].Writes
	if !slices.Equal(got, []string{"Pager.spawned", "Pager.started"}) {
		t.Fatalf("prune: writes %v", got)
	}
	got = dropAttr(p, "Pager.started", "Pager.spawned").Operations[0].Writes
	if !slices.Equal(got, []string{"Pager.spawned", "Pager.spawnedEarly"}) {
		t.Fatalf("dropAttr: writes %v", got)
	}
}

// A verdict recommends what the code follows only when a quote checks out.
func TestCodeVerdictRecommends(t *testing.T) {
	q := func() *Question {
		return &Question{Kind: "conflict", AskOwner: true,
			Requirements: []Requirement{{ID: "a", Title: "Launch the pager", Where: "ADDED in this change"}, {ID: "b", Title: "Fall back without a pager", Where: "current spec"}},
			Options:      []Option{{Choice: "change"}, {Choice: "update_old"}, {Choice: "exception"}, {Choice: "impossible"}}}
	}
	cite := []adjudicate.Cite{{File: "src/main.rs", Line: 40, Valid: true}}
	x := q()
	applyCode(x, adjudicate.Verdict{Kind: "overbroad", Follows: []string{"b"}, Overbroad: []string{"a"}, Reason: "a missing pager falls back", Cites: cite})
	if x.Recommended != "exception" || x.Options[0].Choice != "exception" || x.Code == nil || !strings.Contains(x.Code.Says, "src/main.rs:40") {
		t.Fatalf("overbroad: %+v %+v", x, x.Code)
	}
	// Where one requirement can prevail with no edit to the spec, that is
	// recommended, the too-broad one giving way.
	x = q()
	x.Options = append(x.Options, Option{Choice: "prevails", Prevails: []string{"a"}, Over: []string{"b"}}, Option{Choice: "prevails_other", Prevails: []string{"b"}, Over: []string{"a"}})
	applyCode(x, adjudicate.Verdict{Kind: "overbroad", Follows: []string{"b"}, Overbroad: []string{"a"}, Reason: "a missing pager falls back", Cites: cite})
	if x.Recommended != "prevails_other" || x.Options[0].Choice != "prevails_other" {
		t.Fatalf("overbroad with precedence offered: %+v", x)
	}
	x = q()
	applyCode(x, adjudicate.Verdict{Kind: "superseded", Follows: []string{"a"}, Stale: []string{"b"}, Reason: "it always launches", Cites: cite})
	if x.Recommended != "update_old" || x.Code.Against[0] != "Fall back without a pager" {
		t.Fatalf("superseded: %+v", x)
	}
	x = q()
	applyCode(x, adjudicate.Verdict{Kind: "superseded", Follows: []string{"a"}, Stale: []string{"b"}, Reason: "unchecked", Cites: []adjudicate.Cite{{File: "x", Line: 1}}})
	if x.Recommended != "" || x.Code != nil {
		t.Fatalf("an unchecked quote recommended: %+v", x)
	}
	x = q()
	applyCode(x, adjudicate.Verdict{Kind: "not_implemented", Reason: "no pager code"})
	if x.Recommended != "" || x.Code.Outcome != "not_implemented" {
		t.Fatalf("not implemented: %+v", x)
	}
}

// glossary answers the definitions request with one fact, and translation
// requests as its fake does.
type glossary struct {
	fake
	asked *int
}

func (g glossary) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	if req.System != definePrompt {
		return g.fake.Complete(ctx, req)
	}
	*g.asked++
	if !strings.Contains(req.Turns[0].Text, "Cleared: a resource with no holds") {
		return &llm.Reply{JSON: `{"facts": []}`}, nil
	}
	return &llm.Reply{JSON: `{"facts": [{"term": "Cleared", "on": "Resource.delete", "when": "", "assert": "self.cleared = (self.holds = 0)"}]}`}, nil
}

func TestGlossaryDefinitionSettlesConflict(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nA Cleared resource MUST be deletable.\nGlossary: Cleared: a resource with no holds.\n")
	if err := os.WriteFile(filepath.Join(registry, "vocabulary.yaml"), []byte(strings.Replace(vocabYAML,
		"holds: { type: int, range: [0, 5] }", "holds: { type: int, range: [0, 5] }\n  cleared: { type: bool }", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	asked := 0
	m := glossary{one("Cleared", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "self.cleared"}), &asked}
	out := t.TempDir()
	for run := 1; run <= 2; run++ {
		res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{m}, NoCode: true})
		if err != nil {
			t.Fatal(err)
		}
		if g := res.Gate; g.Check == nil || len(g.Check.Conflicts) != 0 || len(res.Questions) != 1 || res.Questions[0].Kind != "approve" {
			t.Fatalf("run %d: conflicts %+v, questions %+v, warnings %v", run, g.Check, res.Questions, res.Warnings)
		}
	}
	if asked != 1 {
		t.Fatalf("definitions asked %d times, want once", asked)
	}
}

// stalled never answers the definitions request.
type stalled struct {
	fake
	left *time.Duration // the time the definitions request was given
}

func (g stalled) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	if req.System != definePrompt {
		return g.fake.Complete(ctx, req)
	}
	if d, ok := ctx.Deadline(); ok {
		*g.left = time.Until(d)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// The glossary call has the request timeout, not what is left of the run's:
// a stalled one held the command until the run timeout (a code review,
// 2026-11-04).
func TestGlossaryCallHasRequestTimeout(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nA Cleared resource MUST be deletable.\nGlossary: Cleared: a resource with no holds.\n")
	if err := os.WriteFile(filepath.Join(registry, "vocabulary.yaml"), []byte(strings.Replace(vocabYAML,
		"holds: { type: int, range: [0, 5] }", "holds: { type: int, range: [0, 5] }\n  cleared: { type: bool }", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	var left time.Duration
	m := stalled{one("Cleared", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "self.cleared"}), &left}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	res, err := Run(ctx, Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(), Clients: []llm.Client{m}, NoCode: true, RequestTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if left == 0 || left > time.Second || time.Since(start) > 30*time.Second {
		t.Fatalf("definitions given %v, run took %v", left, time.Since(start))
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "definitions for") }) {
		t.Errorf("warnings %v, want the failed definitions", res.Warnings)
	}
}

func TestFallbackReadsAsException(t *testing.T) {
	ids := []string{"pd/FR-002", "pd/FR-005b"}
	general := "The viewer MUST launch the pager when output is a terminal."
	for _, c := range []struct{ b, want string }{
		{"When the pager cannot run, the viewer MUST fall back to printing to stdout.", "pd/FR-005b"},
		{"If the configured pager is not available, output goes to stdout.", "pd/FR-005b"},
		{"If the pager is unavailable, output MUST go to stdout.", "pd/FR-005b"},
		{"The viewer MUST NOT use colour with --color=never.", ""},
		{"The viewer MUST exit with status 0 when writes fail with a broken pipe.", ""},
	} {
		if got := fallbackOf(general, c.b, ids); got != c.want {
			t.Errorf("fallbackOf(%q) = %q, want %q", c.b, got, c.want)
		}
	}
	if got := fallbackOf("Fall back to plain text.", "Fall back to stdout.", ids); got != "" {
		t.Errorf("both fall back: got %q", got)
	}
}

// priced charges a dollar a reply and counts translation requests.
type priced struct {
	fake
	translations *int
}

func (p priced) Complete(ctx context.Context, req llm.Request) (*llm.Reply, error) {
	text := req.Turns[len(req.Turns)-1].Text
	if strings.Contains(text, "statement_id: s1") && !strings.Contains(text, "list its clauses") {
		*p.translations++
	}
	r, err := p.fake.Complete(ctx, req)
	if r != nil {
		r.Usage.CostUSD = 1
	}
	return r, err
}

// A change stopped by its cost budget keeps the translation it paid for: the
// next run finishes without translating the requirement again.
func TestChangeCostBudgetKeepsWork(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nAn erasure request MUST delete the resource.\n")
	n := 0
	m := priced{one("Erasure", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"}), &n}
	out := t.TempDir()
	_, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{m}, NoCode: true, MaxCost: 0.5})
	if err == nil || !strings.Contains(err.Error(), "cost budget") {
		t.Fatalf("capped run: %v", err)
	}
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{m}, NoCode: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Gate.Check == nil || n != 1 {
		t.Fatalf("translated %d times, check %+v", n, res.Gate.Check)
	}
}
