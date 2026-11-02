package translate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/solver"
)

// fixed is a fake model that always answers with the same candidates.
type fixed struct {
	name  string
	cands []Candidate
	mu    sync.Mutex
	calls int
}

func (f *fixed) Name() string { return f.name }

func (f *fixed) Complete(context.Context, llm.Request) (*llm.Reply, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	js, _ := json.Marshal(map[string]any{"rules": f.cands})
	return &llm.Reply{JSON: string(js)}, nil
}

func rule(stmt, kind, on, subject, when, assert string) Candidate {
	return Candidate{StatementID: stmt, Outcome: "rule", Kind: kind, On: on, Subject: subject, Effect: "none",
		When: when, Assert: assert, MissingTerms: []string{}}
}

func TestClusteringFindsAmbiguity(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	vocab := []string{filepath.Join("..", "testdata", "project-a", "registry", "domain.yaml")}
	stmts := []Statement{
		{ID: "S1", Source: "s#L1", Prose: "Stopping or deleting a workspace MUST release its cloud machine"},
		{ID: "S2", Source: "s#L2", Prose: "System MUST NOT automatically stop Workspaces due to idle detection"},
		{ID: "S3", Source: "s#L3", Prose: "System MUST be pleasant to use"},
	}
	never := rule("S2", "invariant", "", "Workspace", "", "not self.autoStoppedForIdle")
	informal := Candidate{StatementID: "S3", Outcome: "informal", Kind: "none", Effect: "none", MissingTerms: []string{}}
	// Model A reads S1 as a step rule on stop; model B as a state invariant.
	a := &fixed{name: "A", cands: []Candidate{rule("S1", "invariant", "Workspace.stop", "", "", "not self.node.vmExists"), never, informal}}
	b := &fixed{name: "B", cands: []Candidate{rule("S1", "invariant", "", "Workspace", "self.status = stopped", "not self.node.vmExists"), never, informal}}

	rep, err := Translate(context.Background(), []llm.Client{a, b}, stmts, Options{VocabFiles: vocab, Samples: 2, Solver: &solver.Z3{}})
	if err != nil {
		t.Fatal(err)
	}
	if a.calls != 2 || b.calls != 2 || len(rep.Samples) != 4 {
		t.Errorf("calls a=%d b=%d samples=%v, want 2, 2 and 4 samples", a.calls, b.calls, rep.Samples)
	}
	got := map[string]StatementClusters{}
	for _, c := range rep.Clusters {
		got[c.StatementID] = c
	}
	s1 := got["S1"]
	if s1.Status != Ambiguous || len(s1.Readings) != 2 || len(s1.Readings[0].Samples) != 2 {
		t.Fatalf("S1 = %+v, want two readings of two samples each", s1)
	}
	if q := s1.Question; q == nil || len(q.Situation) == 0 || q.Says[0] == q.Says[1] {
		t.Errorf("S1 question = %+v, want a situation where the readings disagree", s1.Question)
	}
	if got["S2"].Status != Agreed || got["S3"].Status != Agreed || got["S3"].Readings[0].Outcome != "informal" {
		t.Errorf("S2 = %+v, S3 = %+v, want both agreed", got["S2"], got["S3"])
	}
	for _, c := range rep.Candidates {
		if c.Draft != (c.StatementID == "S1") {
			t.Errorf("%s draft = %v, want draft only for the ambiguous statement", c.RuleID, c.Draft)
		}
	}

	out := t.TempDir()
	if err := WriteRegistry(out, rep, stmts, vocab); err != nil {
		t.Fatal(err)
	}
	rules, _ := os.ReadFile(filepath.Join(out, "rules.yaml"))
	if !strings.Contains(string(rules), "status: draft") {
		t.Errorf("rules.yaml has no draft rule:\n%s", rules)
	}
	q, err := os.ReadFile(filepath.Join(out, "questions.md"))
	if err != nil || !strings.Contains(string(q), "S1") || !strings.Contains(string(q), "They differ when workspace: stop runs, node: vm exists, workspace: status is running; after the step, node: vm exists.") || strings.Contains(string(q), "S2") {
		t.Errorf("questions.md = %q (%v), want one question, about S1, with a situation", q, err)
	}
}

func TestAnswerActivatesReading(t *testing.T) {
	rep := &Report{
		Candidates: []Candidate{{StatementID: "S1", RuleID: "S1", Outcome: "rule", Draft: true}, {StatementID: "S2", RuleID: "S2", Outcome: "rule"}},
		Clusters: []StatementClusters{{StatementID: "S1", Status: Ambiguous, Readings: []Reading{
			{Candidates: []Candidate{{StatementID: "S1", RuleID: "S1", Outcome: "rule", Kind: "invariant", Draft: true}}},
			{Candidates: []Candidate{{StatementID: "S1", RuleID: "S1", Outcome: "rule", Kind: "permission", Draft: true}}},
		}}},
	}
	if err := Answer(rep, "S1", 2); err != nil {
		t.Fatal(err)
	}
	var s1 []Candidate
	for _, c := range rep.Candidates {
		if c.StatementID == "S1" {
			s1 = append(s1, c)
		}
	}
	if len(s1) != 1 || s1[0].Kind != "permission" || s1[0].Draft || rep.Clusters[0].Status != Answered || len(rep.Candidates) != 2 {
		t.Errorf("after answering: candidates %+v, status %s", rep.Candidates, rep.Clusters[0].Status)
	}
	if err := Answer(rep, "S2", 1); err == nil {
		t.Error("answering a statement without a question should fail")
	}
}

func TestRelatedFindsSymbolsByWords(t *testing.T) {
	reg := &ir.Registry{Entities: map[string]*ir.Entity{}, Operations: map[string]*ir.Operation{}}
	ws := &ir.Entity{Name: "Workspace", Attrs: map[string]*ir.Attr{}}
	ws.Attrs["idleWarningShown"] = &ir.Attr{Entity: ws, Name: "idleWarningShown"}
	ws.Attrs["dnsRecordExists"] = &ir.Attr{Entity: ws, Name: "dnsRecordExists"}
	reg.Entities["Workspace"] = ws
	got := Related(reg, "Screens MUST NOT show any idle-shutdown countdown or warning", 5)
	if len(got) == 0 || got[0] != "Workspace.idleWarningShown" {
		t.Errorf("Related = %v, want Workspace.idleWarningShown first", got)
	}
}
