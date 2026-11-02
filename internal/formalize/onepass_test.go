package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

type completionFunc func(llm.Request) (*llm.Reply, error)

func (f completionFunc) Name() string { return "fixture" }
func (f completionFunc) Complete(_ context.Context, r llm.Request) (*llm.Reply, error) {
	return f(r)
}

func onePassFixture(t *testing.T) (string, []translate.Statement, Options) {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "context.yaml")
	if err := os.WriteFile(f, []byte("context: shop\nformatVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, []translate.Statement{
		{ID: "S1", Source: "a.md#L1", Prose: "Orders MUST be paid."},
		{ID: "S2", Source: "b.md#L1", Prose: "Orders MUST be paid before shipping."},
	}, Options{VocabFiles: []string{f}, Context: "shop", OnePass: true, OnePassChunk: 1, NoConsolidate: true, NoCover: true, Rounds: -1}
}

func onePassReply(t *testing.T, vocabulary bool, ids ...string) *llm.Reply {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(`{"entities":[],"extensions":[],"operations":[],"rules":[]}`), &body); err != nil {
		t.Fatal(err)
	}
	if vocabulary {
		body["entities"] = []any{map[string]any{
			"name": "Order", "attributes": []any{map[string]any{"name": "paid", "type": "bool", "set_by": "system"}},
		}}
	}
	var rules []translate.Candidate
	for _, id := range ids {
		rules = append(rules, translate.Candidate{StatementID: id, Outcome: "rule", Kind: "invariant", Assert: "order.paid = true"})
	}
	body["rules"] = rules
	js, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &llm.Reply{JSON: string(js), Usage: llm.Usage{InputTokens: 10, CostUSD: 0.25}}
}

func TestOnePassResumesCompletedChunks(t *testing.T) {
	dir, stmts, opt := onePassFixture(t)
	calls := 0
	interrupted := errors.New("request interrupted")
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		switch calls {
		case 1:
			return onePassReply(t, true, "S1", "S1"), nil
		case 2:
			return nil, interrupted
		case 3:
			if strings.Contains(req.Turns[0].Text, "statement_id: S1") || !strings.Contains(req.System, "paid") {
				t.Errorf("resume did not keep the first chunk: %+v", req)
			}
			return onePassReply(t, false, "S2"), nil
		default:
			t.Fatalf("unexpected model call %d", calls)
			return nil, nil
		}
	})
	if _, err := Run(context.Background(), m, stmts, dir, opt); !errors.Is(err, interrupted) {
		t.Fatalf("first run: %v", err)
	}
	st, err := loadState(dir)
	if err != nil || len(st.Cands) != 2 || st.Usage.InputTokens != 10 {
		t.Fatalf("completed chunk was not saved: %+v, %v", st, err)
	}
	opt.OnePassChunk = 2 // changing chunk size must not replay or skip statements
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[Translated] != 2 || res.Usage.InputTokens != 20 || res.Usage.CostUSD != 0.5 {
		t.Errorf("resumed result: %+v", res)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"S1", "S1.2", "S2"} {
		if reg.Rule(id) == nil {
			t.Errorf("missing rule %s", id)
		}
	}
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("completed run made more model calls: %d", calls)
	}
}

func TestOnePassRepairsIncompleteReplies(t *testing.T) {
	for _, problem := range []string{"missing", "unknown", "malformed", "outcome", "invalid rule", "invalid vocabulary"} {
		t.Run(problem, func(t *testing.T) {
			dir, stmts, opt := onePassFixture(t)
			opt.OnePassChunk = 2
			bad := onePassReply(t, true, "S1", "S2")
			switch problem {
			case "missing":
				bad = onePassReply(t, true, "S1")
			case "unknown":
				bad = onePassReply(t, true, "S1", "S2", "invented")
			case "malformed":
				bad.JSON = "{"
			case "outcome":
				bad.JSON = strings.ReplaceAll(bad.JSON, `"outcome":"rule"`, `"outcome":"unrecognized"`)
			case "invalid rule":
				bad.JSON = strings.ReplaceAll(bad.JSON, "order.paid = true", "order.missing = true")
			case "invalid vocabulary":
				bad.JSON = strings.ReplaceAll(bad.JSON, `"type":"bool"`, `"type":"unknown"`)
			}
			calls := 0
			m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
				calls++
				if calls == 1 {
					return bad, nil
				}
				if calls != 2 || len(req.Turns) != 3 || req.Turns[1].Text != bad.JSON {
					t.Fatalf("expected one repair with the failed reply: %+v", req.Turns)
				}
				return onePassReply(t, true, "S1", "S2"), nil
			})
			res, err := Run(context.Background(), m, stmts, dir, opt)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || res.Counts[Translated] != 2 || len(res.Cands) != 2 || res.Usage.InputTokens != 20 {
				t.Errorf("unrepaired reply accepted: calls=%d, result=%+v", calls, res)
			}
		})
	}
}

func TestOnePassRepairExhaustionIsResumable(t *testing.T) {
	dir, stmts, opt := onePassFixture(t)
	opt.OnePassChunk, opt.MaxRepairs = 2, 1
	calls := 0
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		calls++
		return onePassReply(t, true, "S1", "invented"), nil
	})
	if _, err := Run(context.Background(), m, stmts, dir, opt); err == nil || !strings.Contains(err.Error(), "S2") {
		t.Fatalf("want missing-statement error after repairs, got %v", err)
	}
	st, err := loadState(dir)
	if err != nil || len(st.Cands) != 0 || st.Usage.InputTokens != 20 || calls != 2 {
		t.Fatalf("failed chunk checkpoint: %+v, %v, calls=%d", st, err, calls)
	}
	good := completionFunc(func(llm.Request) (*llm.Reply, error) { return onePassReply(t, true, "S1", "S2"), nil })
	res, err := Run(context.Background(), good, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 2 || res.Usage.InputTokens != 30 {
		t.Fatalf("retry failed: %+v, %v", res, err)
	}
}

func TestRunRejectsChangedInputs(t *testing.T) {
	for _, change := range []string{"prose", "source", "order", "notes", "vocabulary", "context", "mode"} {
		t.Run(change, func(t *testing.T) {
			dir, stmts, opt := onePassFixture(t)
			opt.OnePassChunk = 2
			m := completionFunc(func(llm.Request) (*llm.Reply, error) { return onePassReply(t, true, "S1", "S2"), nil })
			if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, StateFile))
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "prose":
				stmts[0].Prose = "Orders MUST NOT be paid."
			case "source":
				stmts[0].Source = "different.md#L1"
			case "order":
				stmts[0], stmts[1] = stmts[1], stmts[0]
			case "notes":
				stmts[0].Notes = []string{"A corrected reading"}
			case "vocabulary":
				if err := os.WriteFile(opt.VocabFiles[0], []byte("context: changed\nformatVersion: 1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "context":
				opt.Context = "changed"
			case "mode":
				opt.OnePass = false
			}
			never := completionFunc(func(llm.Request) (*llm.Reply, error) { t.Fatal("changed inputs called the model"); return nil, nil })
			if _, err := Run(context.Background(), never, stmts, dir, opt); err == nil || !strings.Contains(err.Error(), "different inputs") {
				t.Errorf("want input mismatch, got %v", err)
			}
			after, _ := os.ReadFile(filepath.Join(dir, StateFile))
			if !reflect.DeepEqual(before, after) {
				t.Error("rejected resume changed its checkpoint")
			}
		})
	}
}

func TestOnePassFindsCrossChunkConflict(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir, stmts, opt := onePassFixture(t)
	stmts[1].Prose = "Orders MUST NOT be paid."
	calls := 0
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		calls++
		if calls == 1 {
			return onePassReply(t, true, "S1"), nil
		}
		reply := onePassReply(t, false, "S2")
		reply.JSON = strings.ReplaceAll(reply.JSON, "order.paid = true", "order.paid = false")
		return reply, nil
	})
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := checker.Check(context.Background(), &solver.Z3{}, prog)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || !reflect.DeepEqual(res.Conflicts[0].Rules, []string{"S1", "S2"}) {
		t.Fatalf("want the conflict spanning both chunks, got %+v", res)
	}
}

func TestRunRejectsLegacyCheckpoint(t *testing.T) {
	dir, stmts, opt := onePassFixture(t)
	if err := os.WriteFile(filepath.Join(dir, StateFile), []byte(`{"stage":"done"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	never := completionFunc(func(llm.Request) (*llm.Reply, error) { t.Fatal("legacy state called the model"); return nil, nil })
	if _, err := Run(context.Background(), never, stmts, dir, opt); err == nil || !strings.Contains(err.Error(), "no input fingerprint") {
		t.Fatalf("want an actionable legacy-state error, got %v", err)
	}
}
