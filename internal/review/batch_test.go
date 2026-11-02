package review

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

func stmt(id, file, prose string) translate.Statement {
	return translate.Statement{ID: id, Source: file + "#L1", Prose: prose}
}

func TestCondense(t *testing.T) {
	prose := "Requirement: Cache Eviction\n古いキャッシュを削除しなければならない（MUST）。削除基準は次の通り。\n- stale: entry が古い\nEviction MAY use access time. Pinned entries MUST NOT be evicted. Context only.\n\n#### Scenario: resumed\n- THEN it MUST choose apply"
	want := "Requirement: Cache Eviction\n古いキャッシュを削除しなければならない（MUST）。\nEviction MAY use access time.\nPinned entries MUST NOT be evicted."
	if got := Condense(prose); got != want {
		t.Errorf("Condense =\n%s\nwant\n%s", got, want)
	}
	if got := Condense("Requirement: Logging\nLogs are kept."); got != "Requirement: Logging\nLogs are kept." {
		t.Errorf("no normative sentence: got %q", got)
	}
}

func TestPlan(t *testing.T) {
	long := strings.Repeat("The change is recorded and the loop continues. ", 20)
	stmts := []translate.Statement{
		stmt("a/1", "a.md", "Requirement: one\nThe system MUST stop. "+long),
		stmt("a/2", "a.md", "Requirement: two\nThe system MUST retry. "+long),
		stmt("b/1", "b.md", "Requirement: three\nThe system SHALL NOT stop. "+long),
	}
	one, _ := Plan(stmts, 0)
	if len(one) != 1 || one[0].Kind != "all" {
		t.Fatalf("budget 0: got %d batches", len(one))
	}
	per := Tokens(stmts[0])
	bs, warn := Plan(stmts, 2*per+5)
	var kinds []string
	for _, b := range bs {
		kinds = append(kinds, fmt.Sprintf("%s:%d", b.Kind, len(b.Statements)))
	}
	if got := strings.Join(kinds, " "); got != "area:2 area:1 condensed:3" || warn != "" {
		t.Errorf("batches = %s (warning %q), want area:2 area:1 condensed:3", got, warn)
	}
	cond := bs[2].Statements[0].Prose
	if strings.Contains(cond, "loop continues") || !strings.Contains(cond, "MUST stop") {
		t.Errorf("condensed statement = %q", cond)
	}
	if _, warn := Plan(stmts, 30); warn == "" {
		t.Error("a condensed corpus over the budget should warn")
	}
}

// fake answers each review with one finding naming the batch's first two
// statements, and fails on a batch whose first statement is "fail".
type fake struct{}

func (fake) Name() string { return "fake" }
func (fake) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) {
	var ids []string
	for _, l := range strings.Split(req.Turns[0].Text, "\n") {
		if id, ok := strings.CutPrefix(l, "statement_id: "); ok {
			ids = append(ids, id)
		}
	}
	if ids[0] == "fail" {
		return nil, fmt.Errorf("boom")
	}
	return &llm.Reply{JSON: fmt.Sprintf(`{"conflicts":[{"statement_ids":[%q,%q],"world_facts":[],"situation":"s"}]}`, ids[1], ids[0]),
		Usage: llm.Usage{CostUSD: 1}}, nil
}

func TestBatchesMerge(t *testing.T) {
	a, b, c := stmt("a", "f", "x"), stmt("b", "f", "y"), stmt("fail", "f", "z")
	batches := []Batch{
		{Kind: "area", Label: "area 1", Statements: []translate.Statement{a, b}},
		{Kind: "condensed", Label: "condensed 1", Statements: []translate.Statement{b, a}}, // same pair, other order
		{Kind: "area", Label: "area 2", Statements: []translate.Statement{c, a}},
	}
	res := Batches(context.Background(), fake{}, batches, nil, 2, nil)
	if len(res.Findings) != 1 || res.Findings[0].Batch != "area" {
		t.Errorf("findings = %+v, want the a+b pair once, from the area call", res.Findings)
	}
	if len(res.Failed) != 1 || !strings.HasPrefix(res.Failed[0], "area 2:") {
		t.Errorf("failed = %v, want area 2", res.Failed)
	}
	if res.Usage.CostUSD != 2 || res.Batches != 3 {
		t.Errorf("usage %v over %d batches, want $2 over 3", res.Usage.CostUSD, res.Batches)
	}
}

func TestNonFindingsAreDropped(t *testing.T) {
	for _, c := range []struct {
		f    Finding
		keep bool
	}{
		{Finding{StatementIDs: []string{"a", "b"}, Situation: "a stop during a retry"}, true},
		{Finding{StatementIDs: []string{"a", "a"}, Situation: "a stop during a retry"}, false},
		{Finding{StatementIDs: []string{"a"}, WorldFacts: []string{"w1"}, Situation: "the disk is gone"}, true},
		{Finding{StatementIDs: []string{"a", "b"}, Situation: "Not a true conflict: b narrows a."}, false},
		{Finding{StatementIDs: []string{"a", "b"}, Situation: "placeholder"}, false},
	} {
		if got := finding(c.f); got != c.keep {
			t.Errorf("finding(%+v) = %v, want %v", c.f, got, c.keep)
		}
	}
}

func TestThinReplyFails(t *testing.T) {
	one := &Result{Findings: []Finding{{StatementIDs: []string{"a", "b"}, Situation: "placeholder"}}}
	if thin(one, 140000, llm.Usage{OutputTokens: 127}) == nil {
		t.Error("one placeholder finding for a 140k prompt should fail")
	}
	if thin(one, 2000, llm.Usage{OutputTokens: 127}) != nil {
		t.Error("a small prompt may have few findings")
	}
	many := &Result{Findings: []Finding{{}, {}}}
	if thin(many, 140000, llm.Usage{OutputTokens: 300}) != nil {
		t.Error("two findings are a review")
	}
}
