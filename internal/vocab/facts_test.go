package vocab

import (
	"context"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// raw is a fake model that answers with the given JSON replies in order.
type raw struct {
	replies []string
	seen    []llm.Request
}

func (r *raw) Name() string { return "raw" }

func (r *raw) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) {
	r.seen = append(r.seen, req)
	js := r.replies[0]
	r.replies = r.replies[1:]
	return &llm.Reply{JSON: js}, nil
}

func TestFactsFirst(t *testing.T) {
	m := &raw{replies: []string{
		// facts, chunk 1: two specs, one fact; plus a restated requirement
		`{"facts": [
		  {"name": "Workspace.idleWarningShown", "meaning": "the idle warning is shown", "type": "bool", "values": [], "min": 0, "max": 0, "statement_ids": ["004/FR-020"]},
		  {"name": "Workspace.minutesUntilShutdown", "meaning": "minutes left before idle shutdown", "type": "int", "values": [], "min": 0, "max": 60, "statement_ids": ["004/FR-020"]}]}`,
		// facts, chunk 2: reuses the fact by name, and adds a synonym
		`{"facts": [
		  {"name": "Workspace.idleWarningShown", "meaning": "", "type": "bool", "values": [], "min": 0, "max": 0, "statement_ids": ["014/FR-009d"]},
		  {"name": "ControlPlane.showsIdleWarnings", "meaning": "warnings are displayed", "type": "bool", "values": [], "min": 0, "max": 0, "statement_ids": ["003/FR-022"]}]}`,
		// consolidation
		`{"merges": [
		  {"from": "ControlPlane.showsIdleWarnings", "into": "Workspace.idleWarningShown", "reason": "same warning"},
		  {"from": "Nope.x", "into": "Workspace.idleWarningShown", "reason": "unknown fact, ignored"}]}`,
		// operations batch
		`{"entities": [], "extensions": [], "operations": [
		  {"name": "Workspace.warnIdle", "statement_ids": ["004/FR-020"], "default": "allow", "writes": ["Workspace.idleWarningShown"], "rationale": ""}]}`,
	}}
	stmts := []translate.Statement{
		{ID: "004/FR-020", Prose: "warn 5 minutes before idle shutdown"},
		{ID: "014/FR-009d", Prose: "MUST NOT display idle warnings"},
		{ID: "003/FR-022", Prose: "display a warning before shutdown"},
	}
	rep, err := Propose(context.Background(), m, stmts, Options{VocabFiles: ctxFile(t), Facts: true, FactBatch: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Errors) != 0 {
		t.Fatal(rep.Errors)
	}
	if len(rep.Facts) != 2 || len(rep.Merges) != 1 {
		t.Fatalf("facts %v merges %v; want 2 facts and 1 merge", rep.Facts, rep.Merges)
	}
	warn := rep.Facts[0]
	if warn.Name != "Workspace.idleWarningShown" || strings.Join(warn.StatementIDs, ",") != "004/FR-020,014/FR-009d,003/FR-022" {
		t.Errorf("warning fact = %+v; want all three statements on it", warn)
	}
	if !strings.Contains(m.seen[1].System, "Workspace.idleWarningShown: bool - the idle warning is shown") {
		t.Error("chunk 2 did not see chunk 1's facts")
	}
	if !strings.Contains(m.seen[3].System, "the idle warning is shown") {
		t.Error("the operations batch did not see fact meanings")
	}
	src, err := YAML(rep.Proposal, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"extend: Workspace", "meaning: the idle warning is shown", "operation: Workspace.warnIdle"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("proposal YAML lacks %q:\n%s", want, src)
		}
	}
	if strings.Contains(string(src), "ControlPlane") {
		t.Errorf("merged fact still proposed:\n%s", src)
	}
}
