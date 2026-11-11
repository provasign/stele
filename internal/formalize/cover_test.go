package formalize

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
)

// coverReply is a clause coverage reply for one statement alias.
func coverReply(alias string, clauses string) *llm.Reply {
	return &llm.Reply{JSON: `{"statements":[{"statement_id":"` + alias + `","clauses":[` + clauses + `]}]}`, Usage: llm.Usage{InputTokens: 1}}
}

// The second statement's rule drops "before shipping": the check names the
// clause, the statement is translated again with it, and the new rules
// cover it.
func TestCoverRetranslatesUncoveredClause(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	var seen []string
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			seen = append(seen, "cover")
			if !strings.Contains(text, "facts:") || !strings.Contains(text, "r1: invariant") {
				t.Errorf("cover request lacks the rules or the facts' meanings:\n%s", text)
			}
			if strings.Contains(text, "s2") {
				// First check: both statements; s2's rule is weaker than its text.
				return &llm.Reply{JSON: `{"statements":[
					{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}]},
					{"statement_id":"s2","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]},{"text":"before shipping","status":"uncovered","rules":[]}],"broader":[{"rules":["r1"],"what":"forbids unpaid orders that never ship"}]}]}`}, nil
			}
			// Second check: only the retranslated statement, now with two rules.
			if !strings.Contains(text, "r2:") {
				t.Errorf("second check should see the new rules:\n%s", text)
			}
			return coverReply("s1", `{"text":"Orders MUST be paid","status":"covered","rules":["r1"]},{"text":"before shipping","status":"covered","rules":["r2"]}`), nil
		case strings.Contains(text, "leave these clauses of the text uncovered"):
			seen = append(seen, "retranslate")
			if !strings.Contains(text, `"before shipping"`) || strings.Contains(text, stmts[0].Prose) {
				t.Errorf("retranslation should name the clause and carry only the uncovered statement:\n%s", text)
			}
			return &llm.Reply{JSON: `{"entities":[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"shipped","type":"bool","values":[],"min":0,"max":0,"set_by":"system","meaning":"the order left the warehouse"}],"relations":[]}],"extensions":[],"operations":[],"rules":[` +
				compactRule("s1", "order.paid = true") + `,{"statement_id":"s1","kind":"invariant","subject":"","on":"","when":"order.shipped = true","assert":"order.paid = true"}]}`}, nil
		default:
			seen = append(seen, "translate")
			return &llm.Reply{JSON: `{"entities":[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system","meaning":"payment was taken"}],"relations":[]}],"extensions":[],"operations":[],"rules":[` +
				compactRule("s1", "order.paid = true") + "," + compactRule("s2", "order.paid = true") + `]}`}, nil
		}
	})
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen, ","); got != "translate,cover,retranslate,cover" {
		t.Fatalf("calls = %s", got)
	}
	if res.Counts[Translated] != 2 || res.Cover != (CoverStats{Statements: 2, Clauses: 3, Covered: 3, Retranslated: 1, Recovered: 1}) {
		t.Fatalf("counts %v, cover %+v", res.Counts, res.Cover)
	}
	var s2 End
	for _, e := range res.Ends {
		if e.ID == stmts[1].ID {
			s2 = e
		}
	}
	if len(s2.Rules) != 2 || len(s2.Uncovered) != 0 {
		t.Fatalf("S2 = %+v, want two rules and no uncovered clause", s2)
	}
	if len(res.Coverage) != 2 || !res.Coverage[1].Retranslated || res.Coverage[1].Clauses[1].Rules[0] != s2.Rules[1] {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	// A finished run makes no further call.
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil || len(seen) != 4 {
		t.Fatalf("rerun: %v, calls %v", err, seen)
	}
}

// A clause still uncovered after retranslation leaves the statement
// partial, naming the clause; a retranslation that fails keeps the old rules.
func TestCoverLeavesUncoveredStatementPartial(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	rereads := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			return &llm.Reply{JSON: `{"statements":[
				{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}]},
				{"statement_id":"s2","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r9"]},{"text":"before shipping","status":"uncovered","rules":[]}]}]}`}, nil
		case strings.Contains(text, "leave these clauses of the text uncovered"):
			rereads++
			return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
		default:
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
		}
	})
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	// A clause left uncovered beside a working rule is not read again: it
	// goes to the owner as text no rule checks.
	if rereads != 0 {
		t.Fatalf("read again %d time(s)", rereads)
	}
	if res.Counts[Translated] != 1 || res.Counts[Partial] != 1 || len(res.Deferred) != 0 {
		t.Fatalf("counts %v, deferred %v", res.Counts, res.Deferred)
	}
	for _, e := range res.Ends {
		if e.ID == stmts[1].ID && (len(e.Rules) != 1 || !slices.Contains(e.Uncovered, "before shipping") || !strings.Contains(e.Reason, `no rule checks "before shipping"`)) {
			t.Fatalf("S2 = %+v", e)
		}
	}
	// "covered" by a rule the statement does not have counts as uncovered.
	if res.Cover.Uncovered != 2 || res.Cover.Covered != 1 || res.Cover.Retranslated != 0 {
		t.Fatalf("cover = %+v", res.Cover)
	}
	if !strings.Contains(Coverage(res), "Clause coverage: 2 statements") || !strings.Contains(Coverage(res), `before shipping`) {
		t.Fatalf("coverage report:\n%s", Coverage(res))
	}
}

// TestNeedsReread: a rule broader than its text, or no working rule, sends
// a statement back; an uncovered clause beside a working rule does not.
func TestNeedsReread(t *testing.T) {
	rule := []translate.Candidate{{Outcome: "rule"}}
	uncovered := StatementCoverage{Clauses: []Clause{{Text: "a", Status: Uncovered}}}
	broader := StatementCoverage{Broader: []Excess{{Rules: []string{"r1"}, What: "x"}}}
	for _, c := range []struct {
		sc    StatementCoverage
		cands []translate.Candidate
		want  bool
	}{
		{uncovered, rule, false},
		{uncovered, nil, true},
		{broader, rule, true},
		{StatementCoverage{}, rule, false},
	} {
		if got := needsReread(c.sc, c.cands); got != c.want {
			t.Errorf("%+v with %d candidate(s): %v", c.sc, len(c.cands), got)
		}
	}
}

// A rule broader than its text goes back with the rule and what it forbids
// named; if the new rules are still broader, the statement stays translated
// with the finding in its reason and in coverage.md.
func TestCoverFlagsBroaderRule(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	var retranslation string
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			if strings.Contains(text, "s2") {
				return &llm.Reply{JSON: `{"statements":[
					{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}],"broader":[{"rules":["r1"],"what":"requires payment of every order, including free ones"}]},
					{"statement_id":"s2","clauses":[{"text":"Orders MUST be paid before shipping","status":"covered","rules":["r1"]}],"broader":[]}]}`}, nil
			}
			return &llm.Reply{JSON: `{"statements":[{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}],"broader":[{"rules":["r1"],"what":"requires payment of free orders"}]}]}`}, nil
		case strings.Contains(text, "forbid what the text allows"):
			retranslation = text
			return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
		default:
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
		}
	})
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(retranslation, "the current rules are: [invariant assert `order.paid = true`]") || !strings.Contains(retranslation, "including free ones") || strings.Contains(retranslation, stmts[1].Prose) {
		t.Fatalf("retranslation should carry S1's current rule and the finding, and only S1:\n%s", retranslation)
	}
	if res.Counts[Translated] != 2 || res.Cover.Broader != 1 || res.Cover.Retranslated != 1 || res.Cover.Recovered != 0 {
		t.Fatalf("counts %v, cover %+v", res.Counts, res.Cover)
	}
	for _, e := range res.Ends {
		if e.ID == stmts[0].ID && (len(e.Broader) != 1 || !strings.Contains(e.Reason, "broader than the text: "+stmts[0].ID+": requires payment of free orders")) {
			t.Fatalf("S1 = %+v", e)
		}
	}
	if md := Coverage(res); !strings.Contains(md, "rules broader than their text (1)") {
		t.Fatalf("coverage.md:\n%s", md)
	}
}

// A statement whose rules contradict each other is read again with the
// clash named; if they still contradict, its rules are held out of the
// registry and it ends invalid with the clash as its reason. A clash
// between two statements is left for the owner.
func TestSelfCheck(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	for _, fixed := range []bool{true, false} {
		dir, stmts, opt := compactFixture(t)
		stmts = append(stmts, translate.Statement{ID: "S3", Source: "a.md#L3", Prose: "Orders MUST NOT be paid."})
		var note string
		m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
			text := req.Turns[0].Text
			if strings.Contains(text, "contradict each other") {
				note = text
				if fixed {
					return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
				}
				return compactFixtureReply(false, compactRule("s1", "order.paid = true")+","+compactRule("s1", "order.paid = false")), nil
			}
			// S2 reads as two rules that cannot both hold; S1 and S3 clash
			// with each other, which is a conflict for the owner.
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")+","+compactRule("s2", "order.paid = false")+","+compactRule("s3", "order.paid = false")), nil
		})
		opt.Batch = 3
		res, err := Run(context.Background(), m, stmts, dir, opt)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(note, "these rules cannot all hold") || !strings.Contains(note, "s1.2: invariant") || strings.Contains(note, stmts[0].Prose) || strings.Contains(note, stmts[2].Prose) {
			t.Fatalf("fixed=%v: the reread should carry only S2 and its clash:\n%s", fixed, note)
		}
		reg, err := ir.LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var s2 End
		for _, e := range res.Ends {
			if e.ID == "S2" {
				s2 = e
			}
		}
		if fixed {
			if res.SelfCheck != (SelfStats{Found: 1, Recovered: 1}) || s2.State != Translated || reg.Rule("S2.2") != nil {
				t.Fatalf("fixed: stats %+v, S2 %+v", res.SelfCheck, s2)
			}
		} else {
			if res.SelfCheck != (SelfStats{Found: 1, Excluded: 1}) || s2.State != Invalid || !strings.Contains(s2.Reason, "its rules contradict each other") || reg.Rule("S2") != nil {
				t.Fatalf("still contradicting: stats %+v, S2 %+v", res.SelfCheck, s2)
			}
		}
		if reg.Rule("S1") == nil || reg.Rule("S3") == nil {
			t.Fatalf("fixed=%v: a clash between two statements must stay for the owner", fixed)
		}
	}
}

// A rule on a step that asserts only what the step does not set blocks the
// step. It is read again with that diagnosis; a precondition says the same
// without a conflict. A rule that still blocks is kept and listed.
func TestSelfCheckBlockingRule(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	vocabulary := `"entities":[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"expired","type":"bool","values":[],"min":0,"max":0,"set_by":"world","meaning":"the order's deadline passed"}],"relations":[]}],"extensions":[],"operations":[{"name":"Order.cancel","statement_ids":["s1"],"default":"allow","writes":[],"rationale":""}]`
	blocking := `{"statement_id":"s1","kind":"invariant","subject":"","on":"Order.cancel","when":"","assert":"not self.expired"}`
	for _, fixed := range []bool{true, false} {
		dir, stmts, opt := compactFixture(t)
		stmts = []translate.Statement{{ID: "S1", Source: "a.md#L1", Prose: "An order MUST NOT be cancelled after it expires."}}
		var note string
		m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
			text := req.Turns[0].Text
			if strings.Contains(text, "can only forbid the step") {
				note = text
				if fixed {
					return &llm.Reply{JSON: `{"entities":[],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"precondition","operation":"Order.cancel","when":"not order.expired"}]}`}, nil
				}
				return &llm.Reply{JSON: `{"entities":[],"extensions":[],"operations":[],"rules":[` + blocking + `]}`}, nil
			}
			return &llm.Reply{JSON: `{` + vocabulary + `,"rules":[` + blocking + `]}`}, nil
		})
		res, err := Run(context.Background(), m, stmts, dir, opt)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(note, "Order.expired (set by the world), which the step does not set") || !strings.Contains(note, "write a precondition") {
			t.Fatalf("fixed=%v: the reread should name the unset fact and the remedy:\n%s", fixed, note)
		}
		e := res.Ends[0]
		if fixed {
			if res.SelfCheck != (SelfStats{Blocking: 1, Unblocked: 1}) || e.State != Translated || e.Blocks != "" {
				t.Fatalf("fixed: stats %+v, end %+v", res.SelfCheck, e)
			}
		} else {
			if res.SelfCheck != (SelfStats{Blocking: 1}) || e.State != Translated || !strings.Contains(e.Reason, "blocks a step") || !strings.Contains(Coverage(res), "rules that block a step (1)") {
				t.Fatalf("still blocking: stats %+v, end %+v", res.SelfCheck, e)
			}
			if reg, err := ir.LoadDir(dir); err != nil || reg.Rule("S1") == nil {
				t.Fatalf("a blocking rule is kept: %v", err)
			}
		}
	}
}

// A rule on a step of the request itself must not make the step write the
// request: requests and actors are inputs, and a registry where a step
// writes one does not load.
func TestNoInferredWriteOfRequest(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts = stmts[:1]
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		return &llm.Reply{JSON: `{"entities":[],"extensions":[{"name":"Request","statement_ids":["s1"],"attributes":[{"name":"via","type":"enum","values":["apiKey","session"],"min":0,"max":0,"set_by":"world"}],"relations":[]}],"operations":[{"name":"Request.authenticate","statement_ids":["s1"],"default":"allow","writes":[],"rationale":""}],"rules":[{"statement_id":"s1","kind":"invariant","subject":"","on":"Request.authenticate","when":"","assert":"self.via = apiKey"}]}`}, nil
	})
	opt.NoSelfCheck = true
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range res.Inferred {
		if strings.Contains(w, "Request.via") {
			t.Fatalf("inferred a write of the request: %v", res.Inferred)
		}
	}
}

// Resuming a finished compact run with no errors, to add the coverage step,
// must not crash on the error map it never saved.
func TestCoverResumesCleanCompactRun(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	reread := false
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		if strings.HasPrefix(req.System, coverPrompt) {
			if reread {
				return coverReply("s1", `{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}`), nil
			}
			return &llm.Reply{JSON: `{"statements":[{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"uncovered","rules":[]}],"broader":[{"rules":["r1"],"what":"x"}]},{"statement_id":"s2","clauses":[{"text":"x","status":"covered","rules":["r1"]}],"broader":[]}]}`}, nil
		}
		if strings.Contains(req.Turns[0].Text, "leave these clauses") {
			reread = true
		}
		return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
	})
	opt.NoSelfCheck = true
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil {
		t.Fatal(err)
	}
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if !reread || res.Cover.Retranslated != 1 || res.Counts[Translated] != 2 {
		t.Fatalf("reread %v, cover %+v, counts %v", reread, res.Cover, res.Counts)
	}
}

// A run stopped in the middle of a reread keeps the statement's earlier
// rules: resuming restores them and asks again.
func TestInterruptedRereadKeepsRules(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := true
	firstJudgments := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			if strings.Contains(text, "s2") {
				firstJudgments++
				return &llm.Reply{JSON: `{"statements":[{"statement_id":"s1","clauses":[{"text":"a","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"before shipping","status":"uncovered","rules":[]}],"broader":[{"rules":["r1"],"what":"x"}]}]}`}, nil
			}
			return coverReply("s1", `{"text":"before shipping","status":"covered","rules":["r1"]}`), nil
		case strings.Contains(text, "leave these clauses"):
			if stop {
				cancel()
				return nil, context.Canceled
			}
			return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
		default:
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
		}
	})
	opt.NoCover, opt.NoSelfCheck = false, true
	if _, err := Run(ctx, m, stmts, dir, opt); err == nil {
		t.Fatal("want the interruption")
	}
	st, err := loadState(dir)
	if err != nil || len(st.Rereading[stmts[1].ID]) != 1 {
		t.Fatalf("the earlier rules of the statement being reread are not in the saved state: %+v, %v", st.Rereading, err)
	}
	stop = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	// The resumed run goes on from the first judgment, not a new one: a
	// stage longer than a check's time slice otherwise never finished.
	if firstJudgments != 1 {
		t.Fatalf("the coverage check judged every statement %d times", firstJudgments)
	}
	if st, _ := loadState(dir); st.CoverProgress != nil {
		t.Fatalf("a finished coverage stage left its progress: %+v", st.CoverProgress)
	}
	if res.Counts[Translated] != 2 || res.Cover.Retranslated != 1 {
		t.Fatalf("after resuming: counts %v, cover %+v", res.Counts, res.Cover)
	}
}

// Only an invariant on a step can be a blocking misreading; a permission
// the step cannot use is a finding about the specs and is left alone.
func TestBlockingRulesAreStepInvariants(t *testing.T) {
	dir := t.TempDir()
	src := `context: shop
formatVersion: 1
---
entity: Invoice
context: shop
attributes:
  archived: { type: bool, setBy: world }
---
operation: Invoice.delete
context: shop
default: allow
writes: []
---
id: P
kind: permission
effect: possible
operation: Invoice.delete
source:
  statementId: S1
---
id: I
kind: invariant
"on": Invoice.delete
assert: self.archived
source:
  statementId: S2
`
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := &checker.Result{Conflicts: []checker.Conflict{{Unit: "Invoice.delete", Rules: []string{"P"}}, {Unit: "Invoice.delete", Rules: []string{"I"}}}}
	got := blockingRules(reg, res, map[string]bool{"S1": true, "S2": true})
	if len(got) != 1 || got["S2"] == nil || !strings.Contains(got["S2"][0], "Invoice.archived (set by the world)") {
		t.Fatalf("blocking = %v, want only the invariant on the step", got)
	}
}

// With --facts-first the facts are proposed over the whole corpus before
// the compact requests, and every request sees them, so two statements in
// different documents use one name for one fact.
func TestCompactFactsFirst(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts[1].Source = "b.md#L1" // another document: another request
	var compactSystems []string
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		switch {
		case strings.Contains(req.Schema, `"facts"`):
			return &llm.Reply{JSON: `{"facts":[{"name":"Order.paid","meaning":"payment was taken","type":"bool","values":[],"min":0,"max":0,"statement_ids":["S1","S2"]}]}`}, nil
		case strings.Contains(req.Schema, `"merges"`):
			return &llm.Reply{JSON: `{"merges":[]}`}, nil
		case strings.Contains(req.Schema, `"rules"`):
			compactSystems = append(compactSystems, sharedVocabulary(req))
			return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
		default: // operations
			return &llm.Reply{JSON: `{"entities":[],"extensions":[],"operations":[]}`}, nil
		}
	})
	opt.FactsFirst, opt.NoSelfCheck = true, true
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(compactSystems) != 2 {
		t.Fatalf("compact requests = %d, want 2", len(compactSystems))
	}
	for _, sys := range compactSystems {
		if !strings.Contains(sys, "paid") || !strings.Contains(sys, "payment was taken") {
			t.Fatal("a compact request did not see the facts proposed over the corpus")
		}
	}
	if res.Counts[Translated] != 2 {
		t.Fatalf("counts = %v", res.Counts)
	}
}

// The owner's lessons reach the translation request as notes and the
// coverage judge as the owner's reading, and the report says which
// statement was read with which lesson; a lesson pinned to another
// statement does not leak.
func TestLessonsReachTranslationAndJudge(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	scope := &ir.Lesson{ID: "L-scope", Kind: ir.LessonScope, Says: "a record checked when it is created is enough here"}
	other := &ir.Lesson{ID: "L-pin", Kind: ir.LessonReading, Says: "read shipping as dispatch", Statements: map[string]string{"S2": translate.TextHash(stmts[1].Prose)}}
	var translated, judged []string
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		if strings.HasPrefix(req.System, coverPrompt) {
			judged = append(judged, req.Turns[0].Text)
			return &llm.Reply{JSON: `{"statements":[{"statement_id":"s1","clauses":[{"text":"a","status":"covered","rules":["r1"]}],"broader":[]},{"statement_id":"s2","clauses":[{"text":"b","status":"covered","rules":["r1"]}],"broader":[]}]}`}, nil
		}
		translated = append(translated, req.Turns[0].Text)
		return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
	})
	opt.NoCover, opt.NoSelfCheck, opt.Lessons = false, true, []*ir.Lesson{scope, other}
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(translated, "\n")
	if !strings.Contains(all, "(L-scope): a record checked") || !strings.Contains(all, "(L-pin): read shipping as dispatch") {
		t.Fatalf("translation request lacks the lessons:\n%s", all)
	}
	if strings.Count(all, "L-pin") != 1 {
		t.Fatalf("the pinned lesson reached another statement:\n%s", all)
	}
	if j := strings.Join(judged, "\n"); !strings.Contains(j, "the owner's reading for this project") || !strings.Contains(j, "a record checked when it is created") {
		t.Fatalf("judge request lacks the owner's reading:\n%s", j)
	}
	got := map[string]string{}
	for _, e := range res.Ends {
		got[e.ID] = strings.Join(e.Lessons, ",")
	}
	if got["S1"] != "L-scope" || got["S2"] != "L-pin,L-scope" {
		t.Fatalf("lessons recorded = %v", got)
	}
	if stmts[0].Notes != nil {
		t.Fatalf("the caller's statements were changed: %v", stmts[0].Notes)
	}
}

// A coverage reread that answers with no rule ("informal: the vocabulary
// already says it") does not replace the statement's rules: it stays
// partial with its rule and the uncovered clause named.
func TestCoverRereadKeepsRulesOverInformal(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			if strings.Contains(text, "s2") {
				return &llm.Reply{JSON: `{"statements":[
					{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}]},
					{"statement_id":"s2","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r2"]},{"text":"before shipping","status":"uncovered","rules":[]}]}]}`}, nil
			}
			return coverReply("s1", `{"text":"Orders MUST be paid","status":"covered","rules":["r1"]},{"text":"before shipping","status":"uncovered","rules":[]}`), nil
		case strings.Contains(text, "leave these clauses of the text uncovered"):
			return compactFixtureReply(false, `{"statement_id":"s1","kind":"informal","rationale":"already captured by the shared vocabulary"}`), nil
		default:
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
		}
	})
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Ends {
		if e.ID == stmts[1].ID && (len(e.Rules) != 1 || e.State != Partial) {
			t.Fatalf("S2 = %+v, want its rule kept, partial", e)
		}
	}
}

// ...and of its old rules it keeps only those within the text: a rule the
// check found broader goes, as the reread was asked to narrow it.
func TestCoverRereadKeepsOnlyRulesWithinText(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			if strings.Contains(text, "s2") {
				return &llm.Reply{JSON: `{"statements":[
					{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}]},
					{"statement_id":"s2","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}],"broader":[{"rules":["r2"],"what":"adds a condition the text lacks"}]}]}`}, nil
			}
			return coverReply("s1", `{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}`), nil
		case strings.Contains(text, "forbid what the text allows"):
			return compactFixtureReply(false, `{"statement_id":"s1","kind":"informal","rationale":"already captured by the shared vocabulary"}`), nil
		default:
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")+","+compactRule("s2", "order.paid = true or order.paid = false")), nil
		}
	})
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Ends {
		if e.ID == stmts[1].ID && (len(e.Rules) != 1 || e.Rules[0] != stmts[1].ID) {
			t.Fatalf("S2 = %+v, want only its rule within the text", e)
		}
	}
}

// A reread the check finds worse (it covers fewer clauses) is undone: the
// earlier rule stands with its coverage.
func TestCoverUndoesWorseReread(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			if strings.Contains(text, "s2") {
				return &llm.Reply{JSON: `{"statements":[
					{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}]},
					{"statement_id":"s2","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]},{"text":"before shipping","status":"uncovered","rules":[]}]}]}`}, nil
			}
			return coverReply("s1", `{"text":"Orders MUST be paid before shipping","status":"uncovered","rules":[]}`), nil
		case strings.Contains(text, "leave these clauses of the text uncovered"):
			return compactFixtureReply(false, compactRule("s1", "order.paid = true or order.paid = false")), nil
		default:
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
		}
	})
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(dir, "rules.yaml"))
	if strings.Contains(string(src), "order.paid = false") {
		t.Fatalf("the worse reread was kept:\n%s", src)
	}
	for _, e := range res.Ends {
		if e.ID == stmts[1].ID && (len(e.Rules) != 1 || len(e.Uncovered) != 1 || e.Uncovered[0] != "before shipping") {
			t.Fatalf("S2 = %+v, want its earlier rule and coverage", e)
		}
	}
}

// A reread that runs to the request limit is sent once: the statement keeps
// its earlier rule.
func TestCoverRereadThatStallsIsNotSentAgain(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	var mu sync.Mutex
	rereads := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		text := req.Turns[0].Text
		switch {
		case strings.HasPrefix(req.System, coverPrompt):
			if strings.Contains(text, "s2") {
				return &llm.Reply{JSON: `{"statements":[
					{"statement_id":"s1","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r1"]}]},
					{"statement_id":"s2","clauses":[{"text":"Orders MUST be paid","status":"covered","rules":["r2"]},{"text":"before shipping","status":"uncovered","rules":[]}],"broader":[{"rules":["r1"],"what":"forbids unpaid orders that never ship"}]}]}`}, nil
			}
			return coverReply("s1", `{"text":"Orders MUST be paid","status":"covered","rules":["r1"]},{"text":"before shipping","status":"uncovered","rules":[]}`), nil
		case strings.Contains(text, "leave these clauses of the text uncovered"):
			mu.Lock()
			rereads++
			mu.Unlock()
			return nil, fmt.Errorf("codex: signal: killed: %w", context.DeadlineExceeded)
		default:
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")), nil
		}
	})
	opt.NoCover = false
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if rereads != 1 {
		t.Errorf("the stalled reread was sent %d times", rereads)
	}
	for _, e := range res.Ends {
		if e.ID == stmts[1].ID && len(e.Rules) != 1 {
			t.Fatalf("S2 = %+v, want its earlier rule", e)
		}
	}
}
