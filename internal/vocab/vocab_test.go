package vocab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// script is a fake model that answers with the given proposals in order and
// records what it was asked.
type script struct {
	replies []Proposal
	seen    []llm.Request
}

func (s *script) Name() string { return "script" }

func (s *script) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) {
	s.seen = append(s.seen, req)
	p := s.replies[0]
	s.replies = s.replies[1:]
	js, _ := json.Marshal(p)
	return &llm.Reply{JSON: string(js)}, nil
}

func ctxFile(t *testing.T) []string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "context.yaml")
	if err := os.WriteFile(f, []byte("context: main\nformatVersion: 1\n---\nentity: Workspace\ncontext: main\nattributes:\n  status: { type: enum, values: [running, stopped] }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return []string{f}
}

func TestProposeRepairsAndFlagsDuplicates(t *testing.T) {
	broken := Proposal{Operations: []Operation{{Name: "Workspace.stop", Default: "allow", Writes: []string{"Workspace.nope"}}}}
	fixed := Proposal{
		Entities: []Entity{{Name: "Node", Attributes: []Attribute{{Name: "vmExists", Type: "bool"}}}},
		Extensions: []Entity{{Name: "Workspace", Attributes: []Attribute{
			{Name: "dnsRecordExists", Type: "bool"},
			{Name: "state", Type: "enum", Values: []string{"stopped", "running"}}, // same values as status
		}, Relations: []Relation{{Name: "node", To: "Node"}}}},
		Operations: []Operation{{Name: "Workspace.stop", Default: "allow", Writes: []string{"Workspace.status", "Workspace.dnsRecordExists"}}},
	}
	second := Proposal{
		Entities:   []Entity{{Name: "Nodes", Attributes: []Attribute{{Name: "ageHours", Type: "int", Min: 0, Max: 48}}}}, // near-duplicate of Node
		Operations: []Operation{{Name: "Workspace.halt", Default: "allow", Writes: []string{"Workspace.dnsRecordExists", "Workspace.status"}}},
	}
	m := &script{replies: []Proposal{broken, fixed, second}}
	stmts := []translate.Statement{{ID: "S1", Source: "s#1", Prose: "stopping removes DNS"}, {ID: "S2", Source: "s#2", Prose: "halting removes DNS"}}
	rep, err := Propose(context.Background(), m, stmts, Options{VocabFiles: ctxFile(t), BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Batches != 2 || rep.Rounds != 3 || len(rep.Errors) != 0 {
		t.Fatalf("batches %d rounds %d errors %v; want 2 batches, 3 rounds (one repair)", rep.Batches, rep.Rounds, rep.Errors)
	}
	if !strings.Contains(m.seen[1].Turns[2].Text, "Workspace.nope") {
		t.Errorf("repair turn lacks the compile error: %s", m.seen[1].Turns[2].Text)
	}
	if !strings.Contains(m.seen[2].System, "dnsRecordExists") {
		t.Error("the second batch did not see the first batch's proposal")
	}
	// Each new symbol is flagged against the one it may duplicate.
	want := map[string]string{"Workspace.state": "Workspace.status", "Nodes": "Node", "Workspace.halt": "Workspace.stop"}
	got := map[string]string{}
	for _, f := range rep.Flags {
		got[f.Symbol] = f.Similar
	}
	for sym, similar := range want {
		if got[sym] != similar {
			t.Errorf("%s flagged against %q, want %q (all: %v)", sym, got[sym], similar, rep.Flags)
		}
	}
	if _, ok := got["Workspace.dnsRecordExists"]; ok {
		t.Errorf("a genuinely new attribute was flagged: %v", rep.Flags)
	}
}

// An enum with the same values but an unrelated name is still flagged.
func TestSameEnumValuesFlagged(t *testing.T) {
	m := &script{replies: []Proposal{{Extensions: []Entity{{Name: "Workspace", Attributes: []Attribute{
		{Name: "lifecycle", Type: "enum", Values: []string{"running", "stopped"}}}}}}}}
	rep, err := Propose(context.Background(), m, []translate.Statement{{ID: "S", Prose: "x"}}, Options{VocabFiles: ctxFile(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Flags) != 1 || rep.Flags[0].Reason != "same enum values on the same entity" {
		t.Errorf("flags = %v", rep.Flags)
	}
}

func TestSameWritesNeedSameVerb(t *testing.T) {
	base := &ir.Registry{Entities: map[string]*ir.Entity{}}
	p := Proposal{Operations: []Operation{
		{Name: "Workspace.stop", Writes: []string{"Workspace.status"}},
		{Name: "Workspace.delete", Writes: []string{"Workspace.status"}},
		{Name: "Workspace.haltIdle", Writes: []string{"Workspace.status"}},
	}}
	var got []string
	for _, f := range duplicates(base, p) {
		got = append(got, f.Symbol+"~"+f.Similar)
	}
	if strings.Join(got, " ") != "Workspace.haltIdle~Workspace.stop" {
		t.Errorf("flags = %v, want only haltIdle against stop", got)
	}
}

func TestOptionalRelationToManyLoads(t *testing.T) {
	p := Proposal{Entities: []Entity{
		{Name: "Theme", Relations: []Relation{{Name: "colors", To: "Color", Many: true, Optional: true}}},
		{Name: "Color", Attributes: []Attribute{{Name: "hex", Type: "string", SetBy: "system"}}},
	}}
	reg, err := Load(Options{Context: "main"}, p)
	if err != nil {
		t.Fatalf("a relation to many marked optional should load: %v", err)
	}
	if r := reg.Entities["Theme"].Relations["colors"]; r == nil || !r.Many || r.Optional {
		t.Fatalf("colors should be a relation to many, not optional: %+v", r)
	}
}
