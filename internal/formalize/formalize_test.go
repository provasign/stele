package formalize

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// router answers each kind of request the pipeline sends with a fixed reply.
type router struct{ translations int }

func (r *router) Name() string { return "router" }

func (r *router) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) {
	text := req.Turns[0].Text
	var js string
	switch {
	case strings.Contains(req.Schema, `"links"`):
		js = `{"merges": [], "links": [{"pair": 1, "when": "", "assert": "order.refunded implies order.status = cancelled", "reason": "only a cancelled order is refunded"},
			{"pair": 1, "when": "", "assert": "order.nothing", "reason": "does not compile"}]}`
	case strings.Contains(req.Schema, `"merges"`):
		js = `{"merges": []}`
	case strings.Contains(req.Schema, `"facts"`):
		js = `{"facts": [
			{"name": "Order.status", "meaning": "the order's state", "type": "enum", "values": ["open", "cancelled"], "min": 0, "max": 0, "set_by": "system", "statement_ids": ["S1"]},
			{"name": "Order.paid", "meaning": "the order is paid", "type": "bool", "values": [], "min": 0, "max": 0, "set_by": "system", "statement_ids": []},
			{"name": "Order.minutesLeft", "meaning": "minutes until the order expires", "type": "int", "values": [], "min": 0, "max": 60, "set_by": "world", "statement_ids": ["S4"]}]}`
	case strings.Contains(req.Schema, `"mappings"`):
		if !strings.Contains(text, "Order.refunded") {
			js = `{"entities": [], "extensions": [], "operations": [], "mappings": []}`
			break
		}
		js = `{"entities": [], "extensions": [{"name": "Order", "statement_ids": ["S2"], "relations": [],
			"attributes": [{"name": "refunded", "type": "bool", "values": [], "min": 0, "max": 0, "set_by": "system"}]}],
			"operations": [], "mappings": [{"statement_id": "S2", "term": "cancel", "symbol": "Order.cancel", "reason": "same action"}, {"statement_id": "S2", "term": "x", "symbol": "Order.nothing", "reason": "invalid"}]}`
	case strings.Contains(req.Schema, `"missing_terms"`):
		r.translations++
		if r.translations > 1 {
			if !strings.Contains(text, `vocabulary note: for "cancel" use Order.cancel`) {
				js = `{"rules": []}`
				break
			}
			js = `{"rules": [{"statement_id": "S2", "rationale": "", "outcome": "rule", "kind": "invariant", "subject": "", "on": "Order.cancel", "operation": "", "effect": "none", "when": "", "assert": "self.refunded = true", "missing_terms": []}]}`
			break
		}
		js = `{"rules": [
			{"statement_id": "S1", "rationale": "", "outcome": "rule", "kind": "invariant", "subject": "", "on": "Order.cancel", "operation": "", "effect": "none", "when": "", "assert": "self.status = cancelled", "missing_terms": []},
			{"statement_id": "S2", "rationale": "no refund fact", "outcome": "needs_vocabulary", "kind": "none", "subject": "", "on": "", "operation": "", "effect": "none", "when": "", "assert": "", "missing_terms": ["Order.refunded"]},
			{"statement_id": "S3", "rationale": "praise", "outcome": "informal", "kind": "none", "subject": "", "on": "", "operation": "", "effect": "none", "when": "", "assert": "", "missing_terms": []},
			{"statement_id": "S4", "rationale": "", "outcome": "rule", "kind": "invariant", "subject": "", "on": "Order.cancel", "operation": "", "effect": "none", "when": "", "assert": "self.minutesLeft > 0", "missing_terms": []},
			{"statement_id": "S5", "rationale": "", "outcome": "rule", "kind": "invariant", "subject": "", "on": "", "operation": "", "effect": "none", "when": "self.paid", "assert": "self.status = open", "missing_terms": []}]}`
	default: // operations
		js = `{"entities": [], "extensions": [], "operations": [
			{"name": "Order.cancel", "statement_ids": ["S1"], "default": "allow", "writes": [], "rationale": ""},
			{"name": "Order.archive", "statement_ids": [], "default": "deny", "writes": ["Order.paid"], "rationale": ""}]}`
	}
	return &llm.Reply{JSON: js}, nil
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	ctxFile := filepath.Join(dir, "context.yaml")
	if err := os.WriteFile(ctxFile, []byte("context: shop\nformatVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stmts := []translate.Statement{
		{ID: "S1", Source: "a.md#L1", Prose: "When an order is cancelled its status MUST be cancelled."},
		{ID: "S2", Source: "b.md#L1", Prose: "Cancelling an order MUST refund it."},
		{ID: "S3", Source: "b.md#L2", Prose: "Orders SHOULD be pleasant."},
		{ID: "S4", Source: "c.md#L1", Prose: "An order MUST NOT be cancelled after it expires."},
		{ID: "S5", Source: "c.md#L2", Prose: "A paid order MUST stay open."},
	}
	m := &router{}
	res, err := Run(context.Background(), m, stmts, dir, Options{VocabFiles: []string{ctxFile}, Context: "shop", NoCover: true})
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]string{}
	for _, e := range res.Ends {
		state[e.ID] = e.State
	}
	if state["S1"] != Translated || state["S2"] != Translated || state["S3"] != Informal || state["S4"] != Translated {
		t.Errorf("end states = %v", state)
	}
	if len(res.Rounds) != 1 || res.Rounds[0].Gaps != 1 || res.Rounds[0].After != 0 || res.Rounds[0].Mappings != 1 {
		t.Errorf("rounds = %+v (the invalid mapping must be dropped)", res.Rounds)
	}
	inferred := strings.Join(res.Inferred, "\n")
	if !strings.Contains(inferred, "Order.cancel writes Order.status") || !strings.Contains(inferred, "Order.cancel writes Order.refunded") || strings.Contains(inferred, "minutesLeft") {
		t.Errorf("inferred writes (never a world-set attribute):\n%s", inferred)
	}
	pruned := strings.Join(res.Pruned, ",")
	// Order.paid is read through self with no subject given: it stays.
	if !strings.Contains(pruned, "operation Order.archive") || strings.Contains(pruned, "Order.paid") {
		t.Errorf("pruned = %s", pruned)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	op := reg.Operations["Order.cancel"]
	if op == nil || len(op.Writes) != 2 || reg.Entities["Order"].Attrs["minutesLeft"].Controller != ir.Environment || !reg.Entities["Order"].Attrs["minutesLeft"].WorldSet {
		t.Errorf("registry: %+v", op)
	}
	if res.Health.SharedAttrs != 1 || res.Health.WithRules != 4 || res.Health.MeetingOtherSrc != 2 {
		t.Errorf("health = %+v", res.Health)
	}
	if len(res.Links) != 1 || reg.Rule("link-1") == nil {
		t.Errorf("want the one link that compiles in the registry: %v", res.Links)
	}
	cov, _ := os.ReadFile(filepath.Join(dir, CoverageFile))
	if !strings.Contains(string(cov), "## informal (1)") {
		t.Errorf("coverage:\n%s", cov)
	}

	// A second run resumes from the saved state and asks the model nothing new.
	before := m.translations
	if _, err := Run(context.Background(), m, stmts, dir, Options{VocabFiles: []string{ctxFile}, Context: "shop", NoCover: true}); err != nil {
		t.Fatal(err)
	}
	if m.translations != before {
		t.Errorf("resume translated again: %d -> %d", before, m.translations)
	}
}

func TestOverrideOfAnotherSpecsLabel(t *testing.T) {
	stmts := []translate.Statement{
		{ID: "005/FR-006", Source: "specs/005-waitlist/spec.md#L40", Prose: "FR-006: A released plot goes to the first member waiting."},
		{ID: "007/FR-006", Source: "specs/007-fee/spec.md#L50", Prose: "FR-006: Paying for an unknown email is rejected."},
		{ID: "007/FR-007", Source: "specs/007-fee/spec.md#L51", Prose: "FR-007: Paying twice is rejected."},
		{ID: "007/FR-008", Source: "specs/007-fee/spec.md#L52", Prose: "FR-008: A released plot goes to the first member waiting who has paid (see 004-release). This is an exception to FR-006 of the waitlist spec (005-waitlist)."},
	}
	cands := []translate.Candidate{
		{StatementID: "007/FR-008", Overrides: []string{"FR-006"}},
		{StatementID: "007/FR-007", Overrides: []string{"FR-006"}},
		{StatementID: "007/FR-008", Overrides: []string{"007/FR-006"}},
	}
	resolveOverrides(stmts, cands)
	if got := cands[0].Overrides; len(got) != 1 || got[0] != "005/FR-006" {
		t.Errorf("named with its spec: overrides = %v, want [005/FR-006]", got)
	}
	if got := cands[1].Overrides; len(got) != 1 || got[0] != "007/FR-006" {
		t.Errorf("named alone: overrides = %v, want [007/FR-006]", got)
	}
	if got := cands[2].Overrides; len(got) != 1 || got[0] != "005/FR-006" {
		t.Errorf("resolved in the wrong spec: overrides = %v, want [005/FR-006]", got)
	}
}
