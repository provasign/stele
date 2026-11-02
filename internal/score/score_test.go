package score

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

var (
	samRegistry   = filepath.Join("..", "testdata", "project-a", "registry")
	samStatements = filepath.Join("..", "testdata", "project-a", "statements.yaml")
	samVocab      = []string{filepath.Join(samRegistry, "domain.yaml")}
)

// replay is a fake LLM that answers with canned replies in order.
type replay struct {
	replies []string
	calls   int
	seen    []llm.Request
}

func (r *replay) Name() string { return "replay" }

func (r *replay) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) {
	r.seen = append(r.seen, req)
	out := r.replies[r.calls]
	r.calls++
	return &llm.Reply{JSON: out}, nil
}

// referenceReply renders the fixture's hand-written rules as a translator reply,
// with edit applied to each candidate.
func referenceReply(t *testing.T, stmts []translate.Statement, edit func(*translate.Candidate)) string {
	t.Helper()
	reg, err := ir.LoadDir(samRegistry)
	if err != nil {
		t.Fatal(err)
	}
	var cands []translate.Candidate
	for _, s := range stmts {
		for _, id := range s.Reference {
			x := reg.Rule(id)
			c := translate.Candidate{StatementID: s.ID, Outcome: "rule", Kind: x.Kind, Subject: x.Subject,
				On: x.On, Operation: x.Operation, Effect: x.Effect, MissingTerms: []string{}}
			if c.Effect == "" {
				c.Effect = "none"
			}
			if x.When != nil {
				c.When = x.When.String()
			}
			if x.Assert != nil {
				c.Assert = x.Assert.String()
			}
			if edit != nil {
				edit(&c)
			}
			cands = append(cands, c)
		}
	}
	js, err := json.Marshal(map[string]any{"rules": cands})
	if err != nil {
		t.Fatal(err)
	}
	return string(js)
}

func run(t *testing.T, replies ...string) (*translate.Report, *Result, *replay) {
	t.Helper()
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	stmts, err := translate.LoadStatements(samStatements)
	if err != nil {
		t.Fatal(err)
	}
	fake := &replay{replies: replies}
	rep, err := translate.Translate(context.Background(), []llm.Client{fake}, stmts, translate.Options{VocabFiles: samVocab})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := translate.WriteRegistry(out, rep, stmts, samVocab); err != nil {
		t.Fatal(err)
	}
	res, err := Score(context.Background(), &solver.Z3{}, samRegistry, out, stmts)
	if err != nil {
		t.Fatal(err)
	}
	return rep, res, fake
}

// A translator that reproduces the hand translation scores perfectly, after
// one repair round for a rule that does not compile.
func TestReferenceTranslationScoresPerfectly(t *testing.T) {
	stmts, err := translate.LoadStatements(samStatements)
	if err != nil {
		t.Fatal(err)
	}
	broken := referenceReply(t, stmts, func(c *translate.Candidate) {
		if c.StatementID == "PA-014-FR009" {
			c.Assert = "not self.node.siblingsDisrupted" // not in the vocabulary
		}
	})
	rep, res, fake := run(t, broken, referenceReply(t, stmts, nil))

	if rep.Rounds != 2 || fake.calls != 2 {
		t.Errorf("rounds = %d, want 2 (one repair)", rep.Rounds)
	}
	if last := fake.seen[1].Turns[2].Text; !strings.Contains(last, "siblingsDisrupted") {
		t.Errorf("repair turn does not show the compile error:\n%s", last)
	}
	if strings.Contains(fake.seen[0].System, "reference") || strings.Contains(fake.seen[0].Turns[0].Text, "reference:") {
		t.Error("the prompt leaks the reference rules")
	}
	if res.Equivalent != res.Total || res.Total != 14 {
		for _, s := range res.Statements {
			t.Logf("%s %s %v", s.Verdict, s.ID, s.Witness)
		}
		t.Errorf("equivalent %d/%d, want 14/14", res.Equivalent, res.Total)
	}
	if len(res.Found) != 8 || len(res.Missed) != 0 || len(res.Extra) != 0 {
		t.Errorf("found %v missed %v extra %v; want 8 found", res.Found, res.Missed, res.Extra)
	}
}

// A translator that reads "in this feature" as a scope and drops FR-009c
// loses that statement and the idle auto-stop conflict, and the scorer says so.
func TestDroppedRuleIsScored(t *testing.T) {
	stmts, err := translate.LoadStatements(samStatements)
	if err != nil {
		t.Fatal(err)
	}
	reply := referenceReply(t, stmts, func(c *translate.Candidate) {
		if c.StatementID == "PA-014-FR009c" {
			*c = translate.Candidate{StatementID: c.StatementID, Outcome: "informal", Kind: "none", Effect: "none", MissingTerms: []string{}}
		}
		if c.StatementID == "PA-004-FR020" {
			c.When = "self.minutesToShutdownDeadline < 5" // off by one
		}
	})
	_, res, _ := run(t, reply)

	verdicts := map[string]string{}
	for _, s := range res.Statements {
		verdicts[s.ID] = s.Verdict
	}
	if verdicts["PA-014-FR009c"] != Untranslated || verdicts["PA-004-FR020"] != Different {
		t.Errorf("verdicts = %v", verdicts)
	}
	if res.Equivalent != 12 {
		t.Errorf("equivalent = %d, want 12", res.Equivalent)
	}
	want := "PA-003-FR023 + PA-014-FR009c"
	if len(res.Missed) != 1 || res.Missed[0] != want {
		t.Errorf("missed = %v, want [%s]", res.Missed, want)
	}
}
