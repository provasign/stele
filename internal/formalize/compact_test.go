package formalize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/lower"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

func compactFixture(t *testing.T) (string, []translate.Statement, Options) {
	dir, stmts, opt := onePassFixture(t)
	opt.OnePass, opt.Compact, opt.Batch = false, true, 20
	stmts[1].Source = "a.md#L2"
	return dir, stmts, opt
}

func compactFixtureReply(vocabulary bool, rules string) *llm.Reply {
	entities := "[]"
	if vocabulary {
		entities = `[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}]`
	}
	return &llm.Reply{JSON: `{"entities":` + entities + `,"extensions":[],"operations":[],"rules":[` + rules + `]}`, Usage: llm.Usage{InputTokens: 10}}
}

func compactRule(id, expr string) string {
	return `{"statement_id":"` + id + `","kind":"invariant","subject":"","on":"","when":"","assert":"` + expr + `"}`
}

// sharedVocabulary is the vocabulary a compact request shows, before its
// statements.
func sharedVocabulary(req llm.Request) string {
	v, _, _ := strings.Cut(req.Turns[0].Text, "# Statements\n")
	return strings.TrimPrefix(req.System, compactPrompt) + "\n" + v
}

func TestCompactRepairsOnlyUnresolvedStatementsAndResumes(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts[0].ID = "long-document.md#requirements/orders/first"
	stmts[1].ID = "long-document.md#requirements/orders/second"
	calls := 0
	interrupted := errors.New("interrupted")
	ctx, cancel := context.WithCancel(context.Background()) // an interruption cancels the run
	defer cancel()
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if strings.Contains(req.Turns[0].Text, stmts[0].ID) || strings.Contains(req.Turns[0].Text, stmts[1].ID) {
			t.Fatal("long provenance IDs leaked into the transport")
		}
		if calls == 1 {
			// S2 has one valid and one invalid part: it must be retried as a unit.
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")+","+compactRule("s2", "order.paid = true")+","+compactRule("s2", "order.missing = false")), nil
		}
		if len(req.Turns) != 1 || strings.Contains(req.Turns[0].Text, stmts[0].Prose) || !strings.Contains(req.Turns[0].Text, "order.missing") || !strings.Contains(sharedVocabulary(req), "paid") {
			t.Fatalf("repair should contain only S2 and its diagnostics: %+v", req)
		}
		if calls == 2 {
			cancel()
			return nil, interrupted
		}
		return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
	})
	if _, err := Run(ctx, m, stmts, dir, opt); !errors.Is(err, interrupted) {
		t.Fatalf("want interruption, got %v", err)
	}
	st, err := loadState(dir)
	if err != nil || len(st.Cands) != 1 || st.Cands[0].StatementID != stmts[0].ID || st.CompactErrors[stmts[1].ID] == "" {
		t.Fatalf("accepted statement or diagnostic lost: %+v, %v", st, err)
	}
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 2 || res.Usage.InputTokens != 20 {
		t.Fatalf("resume: %+v, %v", res, err)
	}
	if res.Calls != (CallStats{Started: 3, Completed: 2, Failed: 1}) {
		t.Fatalf("call count omitted failure/resume: %+v", res.Calls)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil || reg.Rule(stmts[0].ID).StatementID != stmts[0].ID || reg.Rule(stmts[1].ID).Source != stmts[1].Source {
		t.Fatalf("provenance not restored: %+v, %v", reg, err)
	}
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil || calls != 3 {
		t.Fatalf("completed run made another call: %d, %v", calls, err)
	}
}

// A statement that stops at vocabulary it lacks is read once more, told to
// declare it; what the rule language cannot express stays so, with no
// further pass.
func TestCompactKeepsUnsupportedPartsWithoutGapLoop(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.Rounds = 3
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if calls == 2 && !strings.Contains(req.System+req.Turns[len(req.Turns)-1].Text, "The vocabulary is yours to extend") {
			t.Fatal("the second reading is not told to declare the missing vocabulary")
		}
		if calls > 2 {
			t.Fatal("unsupported language feature triggered more than one further model pass")
		}
		s1 := compactRule("s1", "order.paid = true") + `,{"statement_id":"s1","kind":"needs_vocabulary","rationale":"unbounded history is not expressible","missing_terms":["unbounded history"]}`
		if calls == 2 {
			return compactFixtureReply(false, s1), nil // the second reading holds s1 alone
		}
		return compactFixtureReply(true, s1+`,{"statement_id":"s2","kind":"informal","rationale":"no checkable content"}`), nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[NeedsVocabulary] != 1 || res.Counts[Informal] != 1 || len(res.Cands) != 3 {
		t.Fatalf("unsupported part lost: %+v, %v", res, err)
	}
	if calls != 2 {
		t.Fatalf("want one further reading of the statement that needed vocabulary, got %d call(s)", calls)
	}
}

func TestCompactLocalDuplicateDoesNotTriggerRepair(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.Batch = 1
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if calls > 2 {
			t.Fatal("identical repeated vocabulary caused a repair")
		}
		return compactFixtureReply(true, compactRule("s1", "order.paid = true")), nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 2 || len(res.Proposal.Entities) != 1 {
		t.Fatalf("repeated entity was not reused: %+v, %v", res, err)
	}
}

// Replies written in parallel declare the same entities and operations. New
// members are added, writes are combined, and a member defined differently
// keeps the first definition and is reported, never silently replaced.
func TestFoldCompact(t *testing.T) {
	base := vocab.Proposal{
		Entities:   []vocab.Entity{{Name: "Order", Attributes: []vocab.Attribute{{Name: "status", Type: "enum", Values: []string{"open", "paid"}}}}},
		Operations: []vocab.Operation{{Name: "Order.pay", Default: "allow", Writes: []string{"Order.status"}}},
	}
	reply := vocab.Proposal{
		Entities: []vocab.Entity{
			{Name: "Order", Attributes: []vocab.Attribute{
				{Name: "status", Type: "enum", Values: []string{"paid", "open"}, Meaning: "worded differently"}, // same definition
				{Name: "total", Type: "int", Min: 0, Max: 100},                                                  // new member
			}},
			{Name: "Invoice", Attributes: []vocab.Attribute{{Name: "sent", Type: "bool"}}},
		},
		Operations: []vocab.Operation{
			{Name: "Order.pay", Default: "allow", Writes: []string{"Order.total"}},
			{Name: "Invoice.send", Default: "allow", Writes: []string{"Invoice.sent"}},
		},
	}
	got, clashes := foldCompact(base, reply, nil)
	if len(clashes) != 0 || len(got.Entities) != 2 || len(got.Entities[0].Attributes) != 2 || len(got.Operations) != 2 ||
		!reflect.DeepEqual(got.Operations[0].Writes, []string{"Order.status", "Order.total"}) {
		t.Fatalf("got %+v, clashes %v", got, clashes)
	}
	if len(base.Entities[0].Attributes) != 1 || len(base.Operations[0].Writes) != 1 {
		t.Fatal("folding changed the base proposal")
	}
	// Partial views of one enum and range are widened; world-set writes dropped.
	wider := vocab.Proposal{
		Entities: []vocab.Entity{{Name: "Order", Attributes: []vocab.Attribute{
			{Name: "status", Type: "enum", Values: []string{"open", "refunded"}},
			{Name: "seen", Type: "bool", SetBy: "world"},
		}}},
		Operations: []vocab.Operation{{Name: "Order.refund", Default: "allow", Writes: []string{"Order.status", "Order.seen", "Request.refunded"}}},
	}
	got, clashes = foldCompact(base, wider, nil)
	if len(clashes) != 0 || !reflect.DeepEqual(got.Entities[0].Attributes[0].Values, []string{"open", "paid", "refunded"}) ||
		!reflect.DeepEqual(got.Operations[1].Writes, []string{"Order.status"}) {
		t.Fatalf("not widened: %+v, %v", got, clashes)
	}
	clash := vocab.Proposal{
		Entities:   []vocab.Entity{{Name: "Order", Attributes: []vocab.Attribute{{Name: "status", Type: "bool"}}}},
		Operations: []vocab.Operation{{Name: "Order.pay", Default: "deny"}},
	}
	got, clashes = foldCompact(base, clash, nil)
	if !reflect.DeepEqual(clashes, []string{"Order.status", "Order.pay"}) || got.Entities[0].Attributes[0].Type != "enum" || got.Operations[0].Default != "allow" {
		t.Fatalf("clashing definitions replaced: %+v, %v", got, clashes)
	}
}

func TestCompactFindsCrossDocumentConflict(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	dir, stmts, opt := compactFixture(t)
	stmts[1].Source, stmts[1].Prose = "other.md#L1", "Orders MUST NOT be paid."
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if calls == 1 {
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")), nil
		}
		if !strings.Contains(sharedVocabulary(req), "paid") {
			t.Fatal("shared vocabulary missing")
		}
		return compactFixtureReply(false, compactRule("s1", "order.paid = false")), nil
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
	if err != nil || len(res.Conflicts) != 1 || !reflect.DeepEqual(res.Conflicts[0].Rules, []string{"S1", "S2"}) {
		t.Fatalf("cross-document contradiction lost: %+v, %v", res, err)
	}
}

func TestCompactRejectsLossyOrInventedReplies(t *testing.T) {
	for _, rule := range []string{
		`{"statement_id":"s1","kind":"informal","rationale":"x","when":"order.paid"}`,
		`{"statement_id":"s1","kind":"invariant","subject":"","on":"","assert":"order.paid"}`,
		compactRule("unknown", "order.paid"),
		`{"statement_id":"s1","kind":"permission","operation":"Order.pay","effect":"none","when":""}`,
		`{"statement_id":"s1","kind":"invariant","subject":"","on":"","when":null,"assert":"order.paid"}`,
	} {
		if _, err := decodeCompact(compactFixtureReply(false, rule).JSON, map[string]string{"s1": "S1"}); err == nil {
			t.Errorf("accepted invalid reply: %s", rule)
		}
	}
}

func TestCompactRejectsOverwrittenVocabulary(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.MaxRepairs = -1
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		r := compactFixtureReply(true, compactRule("s1", "order.paid"))
		r.JSON = strings.Replace(r.JSON, `"attributes":[`, `"attributes":[{"name":"paid","type":"int","min":0,"max":3},`, 1)
		return r, nil
	})
	res, err := Run(context.Background(), m, stmts[:1], dir, opt)
	if err != nil || res.Counts[Invalid] != 1 || !strings.Contains(res.Deferred[stmts[0].ID], "duplicate attribute") {
		t.Fatalf("silently overwrote a definition: %+v, %v", res, err)
	}
	st, _ := loadState(dir)
	if len(st.Cands) != 0 || len(st.Proposal.Entities) != 0 {
		t.Fatal("invalid definitions were checkpointed")
	}
}

func TestCompactDefersInvalidStatementAndReachesLaterDocument(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts[1].Source = "later.md#L1"
	opt.MaxRepairs = 1
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if strings.Contains(req.Turns[0].Text, stmts[0].Prose) {
			return compactFixtureReply(true, compactRule("s1", "order.unknown")), nil
		}
		return compactFixtureReply(false, compactRule("s1", "order.paid")), nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || res.Counts[Invalid] != 1 || res.Counts[Translated] != 1 || len(res.Deferred) != 1 || !strings.Contains(res.Deferred[stmts[0].ID], "unknown") {
		t.Fatalf("deferred statement blocked or disappeared: calls=%d, %+v", calls, res)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil || reg.Rule(stmts[0].ID) != nil || reg.Rule(stmts[1].ID) == nil {
		t.Fatalf("deferred rules emitted, or later rule missing: %+v, %v", reg, err)
	}
	coverage, err := os.ReadFile(filepath.Join(dir, CoverageFile))
	if err != nil || !strings.Contains(string(coverage), "deferred for review") || !strings.Contains(string(coverage), "unknown") {
		t.Fatalf("review diagnostic missing: %s, %v", coverage, err)
	}
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil || calls != 3 {
		t.Fatalf("resume retried deferred work implicitly: %d, %v", calls, err)
	}
}

func TestInvalidPartCannotBeReportedAsFullyTranslated(t *testing.T) {
	stmts := []translate.Statement{{ID: "S", Prose: "two obligations"}}
	cands := []translate.Candidate{{StatementID: "S", RuleID: "S", Outcome: "rule"}, {StatementID: "S", RuleID: "S.2", Outcome: "rule", Error: "unsupported obligation"}}
	ends, counts := ends(stmts, cands, nil, nil, nil, nil)
	if counts[Invalid] != 1 || ends[0].State != Invalid || !strings.Contains(ends[0].Reason, "unsupported obligation") {
		t.Fatalf("invalid part hidden: %+v", ends)
	}
}

func TestCompactDeferredSurvivesInterruptedLaterDocument(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.MaxRepairs = -1
	stmts[1].Source = "later.md#L1"
	calls := 0
	interrupted := errors.New("interrupted later request")
	ctx, cancel := context.WithCancel(context.Background()) // an interruption cancels the run
	defer cancel()
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if calls == 1 {
			return compactFixtureReply(true, compactRule("s1", "order.unknown")), nil
		}
		if strings.Contains(req.Turns[0].Text, stmts[0].Prose) {
			t.Fatal("retried deferred statement")
		}
		if calls == 2 {
			cancel()
			return nil, interrupted
		}
		return compactFixtureReply(false, compactRule("s1", "order.paid")), nil
	})
	if _, err := Run(ctx, m, stmts, dir, opt); !errors.Is(err, interrupted) {
		t.Fatalf("want interruption: %v", err)
	}
	st, err := loadState(dir)
	if err != nil || len(st.CompactDeferred) != 1 {
		t.Fatalf("deferred diagnostic lost: %+v, %v", st, err)
	}
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || calls != 3 || res.Counts[Invalid] != 1 || res.Counts[Translated] != 1 {
		t.Fatalf("resume: %+v, %v, calls=%d", res, err, calls)
	}
}

func TestRecordedEnumFailuresCompileWithoutModel(t *testing.T) {
	data, err := recorded(t, "STELE_RECORDED_TRIAL")
	if err != nil {
		t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	dir, _, opt := compactFixture(t)
	files, err := writeVocabulary(dir, st.Proposal, opt)
	if err != nil {
		t.Fatal(err)
	}
	cands := []translate.Candidate{
		{StatementID: "publish", RuleID: "publish", Outcome: "rule", Kind: "invariant", On: "Model.publish", When: "self.engine = vllm and self.sizingFitsTargetArchitecture", Assert: "self.published and self.publishResponseShape = existing and self.fitCheckAnnotationPresent and self.auditEventEmitted and self.auditFitCheckStatus = self.fitCheckStatus"},
		{StatementID: "status", RuleID: "status", Outcome: "rule", Kind: "invariant", On: "Reconciler.handleBackendRestart", Assert: "self.reportedDeploymentStatus = self.deployment.status"},
	}
	translate.Revalidate(files, cands)
	for _, c := range cands {
		if c.Error != "" {
			t.Errorf("recorded expression still needs model repair: %s", c.Error)
		}
	}
}

func TestCompactBatchesBySourceBytesAndKeepsInformalStatements(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.BatchBytes = 1 // even an oversized statement must make forward progress
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if strings.Count(req.Turns[0].Text, "statement_id:") != 1 {
			t.Fatal("source byte cap ignored")
		}
		return compactFixtureReply(false, `{"statement_id":"s1","kind":"informal","rationale":"no checkable content"}`), nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || calls != 2 || res.Counts[Informal] != 2 {
		t.Fatalf("informal corpus: %+v, %v", res, err)
	}
}

func TestCompactBudgetAndModeBinding(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.MaxCalls, opt.Batch = 1, 1
	calls := 0
	m := completionFunc(func(llm.Request) (*llm.Reply, error) {
		calls++
		return compactFixtureReply(true, compactRule("s1", "order.paid")), nil
	})
	for range 2 {
		if _, err := Run(context.Background(), m, stmts, dir, opt); err == nil || !strings.Contains(err.Error(), "budget reached") {
			t.Fatalf("budget not enforced: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("resume bypassed budget: %d", calls)
	}
	opt.Compact = false
	if _, err := Run(context.Background(), m, stmts, dir, opt); err == nil || !strings.Contains(err.Error(), "different inputs") {
		t.Fatalf("mode changed under checkpoint: %v", err)
	}
}

func TestCompactBudgetCannotSilentlySkipConsolidation(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.NoConsolidate, opt.MaxCalls = false, 1
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		if strings.Contains(req.Schema, `"links"`) {
			return &llm.Reply{JSON: `{"merges":[],"links":[]}`}, nil
		}
		r := compactFixtureReply(true, compactRule("s1", "order.paid = order.paymentReceived"))
		r.JSON = strings.Replace(r.JSON, `"attributes":[`, `"attributes":[{"name":"paymentReceived","type":"bool","values":[],"min":0,"max":0,"set_by":"system"},`, 1)
		return r, nil
	})
	if _, err := Run(context.Background(), m, stmts[:1], dir, opt); err == nil || !strings.Contains(err.Error(), "budget reached") {
		t.Fatalf("incomplete consolidation reported success: %v", err)
	}
	st, err := loadState(dir)
	if err != nil || st.Stage != "translated" || st.Consolidated {
		t.Fatalf("incomplete consolidation checkpointed: %+v, %v", st, err)
	}
	// The translation was written before consolidation began.
	if reg, err := ir.LoadDir(dir); err != nil || len(reg.Rules) != 1 {
		t.Fatalf("no registry left by the stopped run: %v", err)
	}
	var early Result
	if js, err := os.ReadFile(filepath.Join(dir, "formalize.json")); err != nil || json.Unmarshal(js, &early) != nil || early.Consolidated || early.Counts[Translated] != 1 {
		t.Fatalf("report before consolidation: %+v, %v", early, err)
	}
	opt.MaxCalls = 2
	if _, err := Run(context.Background(), m, stmts[:1], dir, opt); err != nil {
		t.Fatalf("consolidation did not resume: %v", err)
	}
}

// This is an offline encoding audit, not a translation-accuracy or speed test.
// It re-encodes the saved experiment if present without invoking any model.
func TestCompactSavedPayloadAudit(t *testing.T) {
	data, err := recorded(t, "STELE_RECORDED_PAYLOAD")
	if err != nil {
		t.Fatal(err)
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	var original, compact []map[string]any
	aliases := map[string]string{}
	for _, c := range st.Cands {
		if aliases[c.StatementID] == "" {
			aliases[c.StatementID] = fmt.Sprintf("s%d", len(aliases)+1)
		}
		full := map[string]any{"statement_id": c.StatementID, "rationale": c.Rationale, "outcome": c.Outcome, "kind": c.Kind, "subject": c.Subject, "on": c.On, "operation": c.Operation, "effect": c.Effect, "when": c.When, "assert": c.Assert, "missing_terms": c.MissingTerms}
		original = append(original, full)
		kind := c.Kind
		if c.Outcome != "rule" {
			kind = c.Outcome
		}
		small := map[string]any{"statement_id": aliases[c.StatementID], "kind": kind}
		for _, v := range compactKinds {
			if v.kind == kind {
				for _, k := range v.keys {
					small[k] = full[k]
				}
			}
		}
		compact = append(compact, small)
	}
	full, _ := json.Marshal(original)
	small, _ := json.Marshal(compact)
	body := map[string]any{"entities": []any{}, "extensions": []any{}, "operations": []any{}, "rules": compact}
	encoded, _ := json.Marshal(body)
	byAlias := map[string]string{}
	for id, alias := range aliases {
		byAlias[alias] = id
	}
	decoded, err := decodeCompact(string(encoded), byAlias)
	if err != nil {
		t.Fatal(err)
	}
	for i, got := range decoded.Rules {
		want := st.Cands[i]
		if got.StatementID != want.StatementID || got.Outcome != want.Outcome || got.Kind != want.Kind || got.Subject != want.Subject || got.On != want.On || got.Operation != want.Operation || got.When != want.When || got.Assert != want.Assert || (got.Kind == "permission" && got.Effect != want.Effect) || !reflect.DeepEqual(got.MissingTerms, want.MissingTerms) && len(got.MissingTerms)+len(want.MissingTerms) > 0 {
			t.Fatalf("compact transport changed entry %d: %+v vs %+v", i, got, want)
		}
	}
	t.Logf("%d saved entries: %d original rule bytes; %d compact rule bytes; %.1f%% reduction (vocabulary excluded)", len(st.Cands), len(full), len(small), 100*(1-float64(len(small))/float64(len(full))))
}

// Three documents go out at once against the same starting vocabulary. Two
// replies define Order.paid identically; the third gives it another type, so
// the first definition stands and that statement alone, whose rule no longer
// compiles, is repaired in a second wave against the merged vocabulary. The merge follows input order, not completion order.
func TestCompactParallelWaves(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts = append(stmts[:1], translate.Statement{ID: "S2", Source: "b.md#L1", Prose: "Orders MUST be paid before shipping."},
		translate.Statement{ID: "S3", Source: "c.md#L1", Prose: "Paid orders MUST be shippable."})
	opt.Parallel = 3
	var mu sync.Mutex
	inFlight, peak, calls := 0, 0, 0
	all := make(chan struct{})
	clash := `{"entities":[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"paid","type":"enum","values":["no","yes"],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[` + compactRule("s1", "order.paid = yes") + `]}`
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		mu.Lock()
		calls++
		n := calls
		inFlight++
		peak = max(peak, inFlight)
		if inFlight == 3 {
			close(all)
		}
		mu.Unlock()
		defer func() { mu.Lock(); inFlight--; mu.Unlock() }()
		text := req.Turns[0].Text
		if !strings.HasPrefix(req.System, compactPrompt) {
			t.Error("every request must start with the translation prompt")
		}
		if n <= 3 {
			select { // the first wave must be in flight together
			case <-all:
			case <-time.After(5 * time.Second):
				t.Error("first wave was not concurrent")
			}
			if strings.Contains(text, "c.md") {
				time.Sleep(20 * time.Millisecond) // finishes last but is merged by input order
				return &llm.Reply{JSON: clash}, nil
			}
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")), nil
		}
		if !strings.Contains(text, "c.md") || !strings.Contains(text, "kept its definitions of Order.paid") || !strings.Contains(sharedVocabulary(req), "paid") {
			t.Errorf("second wave should repair only S3 against the merged vocabulary: %s", text)
		}
		return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 3 {
		t.Fatalf("%+v, %v", res, err)
	}
	if peak != 3 || calls != 4 {
		t.Errorf("peak %d in flight over %d calls; want 3 over 4", peak, calls)
	}
	var got []string
	for _, c := range res.Cands {
		got = append(got, c.StatementID)
	}
	if !reflect.DeepEqual(got, []string{"S1", "S2", "S3"}) {
		t.Errorf("candidates out of input order: %v", got)
	}
}

func TestPackCompact(t *testing.T) {
	s := func(id, src string, n int) translate.Statement {
		return translate.Statement{ID: id, Source: src, Prose: strings.Repeat("x", n)}
	}
	parts := packCompact([]translate.Statement{
		s("a1", "a.md#1", 10), s("b1", "b.md#1", 10), s("a2", "a.md#2", 10), s("a3", "a.md#3", 10), s("c1", "c.md#1", 10),
	}, 2, 1000, 3, false, nil)
	var got [][]string
	for _, p := range parts {
		var ids []string
		for _, x := range p {
			ids = append(ids, x.ID)
		}
		got = append(got, ids)
	}
	// a3 opens the third batch; c1 waits for the next wave.
	if want := [][]string{{"a1", "a2"}, {"b1"}, {"a3"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if p := packCompact([]translate.Statement{s("a1", "a.md#1", 10), s("b1", "b.md#1", 10)}, 5, 1000, 1, true, nil); len(p) != 1 || len(p[0]) != 2 {
		t.Errorf("across documents, one batch should hold both: %v", p)
	}
	if p := packCompact([]translate.Statement{s("big", "a.md#1", 50), s("next", "a.md#2", 1)}, 5, 20, 2, false, nil); len(p) != 2 || len(p[0]) != 1 {
		t.Errorf("an oversized statement must stay alone and intact: %v", p)
	}
	// A repair or reread note counts towards the limit; a backend failure does not.
	two := []translate.Statement{s("a1", "a.md#1", 10), s("a2", "a.md#2", 10)}
	if p := packCompact(two, 5, 40, 2, false, map[string]string{"a2": backendFailed + "timed out"}); len(p) != 1 {
		t.Errorf("without a note both fit in 40 bytes: %v", p)
	}
	if p := packCompact(two, 5, 40, 2, false, map[string]string{"a2": "the current rules are: ..."}); len(p) != 2 {
		t.Errorf("with its reread note a2 no longer fits: %v", p)
	}
}

// A reply may propose every D-050 type: strings, record fields (dotted
// names), lists and relations to many, with rules using functions and
// quantifiers. The registry is written, loads, and the checker finds the
// contradiction that spans instances.
func TestCompactNewTypesEndToEnd(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts[0].Prose, stmts[1].Prose = "Stopping a workspace deletes its node's VM.", "A node keeps its VM while any of its workspaces runs."
	reply := `{"entities":[
	{"name":"Node","statement_ids":["s1","s2"],"attributes":[{"name":"vmExists","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],
	 "relations":[{"name":"workspaces","to":"Workspace","many":true}]},
	{"name":"Workspace","statement_ids":["s1","s2"],"attributes":[
	  {"name":"status","type":"enum","values":["running","stopped"],"min":0,"max":0,"set_by":"system"},
	  {"name":"route","type":"string","values":[],"min":0,"max":0,"set_by":"system"},
	  {"name":"result.code","type":"int","values":[],"min":0,"max":2,"set_by":"system"},
	  {"name":"tags","type":"list<enum[hot, cold]>","values":[],"min":0,"max":0,"set_by":"system"}],
	 "relations":[{"name":"node","to":"Node","many":false}]}],
	"extensions":[],
	"operations":[{"name":"Workspace.stop","statement_ids":["s1"],"default":"allow","writes":["Workspace.status","Node.vmExists","Workspace.result.code"],"rationale":""}],
	"rules":[
	{"statement_id":"s1","kind":"invariant","subject":"","on":"Workspace.stop","when":"","assert":"not self.node.vmExists and self.result.code = 0"},
	{"statement_id":"s2","kind":"invariant","subject":"","on":"","when":"","assert":"all(w in node.workspaces: w.status = running implies node.vmExists)"},
	{"statement_id":"s2","kind":"invariant","subject":"","on":"","when":"","assert":"startsWith(workspace.route, \"/w/\") and len(workspace.tags) <= 3"}]}`
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) { return &llm.Reply{JSON: reply}, nil })
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 2 {
		t.Fatalf("%+v, %v", res, err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a := reg.Entities["Workspace$result"]; a == nil || a.Attrs["code"] == nil || !reg.Entities["Node"].Relations["workspaces"].Many {
		t.Fatalf("record or relation to many lost: %+v", reg.Entities["Node"].Relations)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := checker.Check(context.Background(), &solver.Z3{}, prog)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range got.Conflicts {
		found = found || (c.Unit == "Workspace.stop" && reflect.DeepEqual(c.Rules, []string{"S1", "S2"}))
	}
	if !found {
		t.Fatalf("cross-instance conflict not found: %+v", got.Conflicts)
	}
}

// A request that fails on its own (a crashed CLI, its own deadline) is a
// failed attempt for its statements: they are retried and the run goes on.
// The backend error is not shown to the model.
func TestCompactRetriesFailedRequest(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.Batch = 1
	calls := 0
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("codex: signal: killed")
		}
		if strings.Contains(req.Turns[0].Text, "killed") {
			t.Error("backend error shown to the model")
		}
		return compactFixtureReply(true, compactRule("s1", "order.paid = true")), nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 2 || res.Calls.Failed != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
}

// One invalid vocabulary member (a collection of collections) and a write of
// an undeclared attribute must not reject the reply: the rest is kept, and
// only the statement using the invalid member is repaired.
func TestCompactSalvagesVocabulary(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	opt.MaxRepairs = -1 // defer after the first reply, so the test makes one call
	reply := `{"entities":[{"name":"Order","statement_ids":["s1","s2"],"attributes":[
	  {"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"},
	  {"name":"query","type":"map<string, list<string>>","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],
	"extensions":[],
	"operations":[{"name":"Order.pay","statement_ids":["s1"],"default":"allow","writes":["Order.paid","Order.missing"],"rationale":""}],
	"rules":[` + compactRule("s1", "order.paid = true") + `,` + compactRule("s2", `has(order.query, \"a\")`) + `]}`
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) { return &llm.Reply{JSON: reply}, nil })
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 1 || res.Counts[Invalid] != 1 {
		t.Fatalf("%+v, %v", res.Counts, err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Order.pay itself is pruned (no rule names it); its proposal kept only
	// the declared write.
	if o := reg.Entities["Order"]; o.Attrs["paid"] == nil || o.Attrs["query"] != nil {
		t.Fatalf("salvage kept the wrong members: %v", o.AttrNames())
	}
	for _, op := range res.Proposal.Operations {
		if op.Name == "Order.pay" && !reflect.DeepEqual(op.Writes, []string{"Order.paid"}) {
			t.Fatalf("writes %v", op.Writes)
		}
	}
}

// Adding a spec to an existing registry: each request shows only the
// related existing vocabulary, the new rules reuse it, and the combined
// registry finds a conflict between a base rule and a new one.
func TestCompactRetrievesBaseVocabulary(t *testing.T) {
	dir := t.TempDir()
	base := `context: shop
formatVersion: 1
---
entity: Pager
context: shop
attributes:
  exitCode: { type: int, range: [0, 255], meaning: the status the application exits with }
  brokenPipe: { type: bool }
---
entity: Theme
context: shop
attributes:
  palette: { type: string }
---
operation: Pager.close
context: shop
default: allow
writes: [Pager.exitCode]
`
	if err := os.WriteFile(filepath.Join(dir, "base-vocabulary.yaml"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	baseRules := `id: OLD
kind: invariant
on: Pager.close
when: self.brokenPipe
assert: self.exitCode = 141
`
	if err := os.WriteFile(filepath.Join(dir, "base-rules.yaml"), []byte(baseRules), 0o644); err != nil {
		t.Fatal(err)
	}
	stmts := []translate.Statement{{ID: "NEW", Source: "pager.md#L1", Prose: "When the pager exits early and the write fails with a broken pipe, the application MUST exit with status 0."}}
	opt := Options{VocabFiles: []string{filepath.Join(dir, "base-vocabulary.yaml")}, Context: "shop", Compact: true, Retrieve: 1, NoConsolidate: true, NoCover: true, Rounds: -1}
	m := completionFunc(func(req llm.Request) (*llm.Reply, error) {
		if v := sharedVocabulary(req); !strings.Contains(v, "exitCode") || strings.Contains(v, "palette") {
			t.Errorf("prompt should show the related exitCode, not the unrelated palette")
		}
		return &llm.Reply{JSON: `{"entities":[],"extensions":[],"operations":[],"rules":[` + `{"statement_id":"s1","kind":"invariant","subject":"","on":"Pager.close","when":"self.brokenPipe","assert":"self.exitCode = 0"}` + `]}`}, nil
	})
	res, err := Run(context.Background(), m, stmts, dir, opt)
	if err != nil || res.Counts[Translated] != 1 {
		t.Fatalf("%+v, %v", res.Counts, err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prog, err := lower.Lower(reg, lower.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := checker.Check(context.Background(), &solver.Z3{}, prog)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Conflicts) != 1 || !reflect.DeepEqual(got.Conflicts[0].Rules, []string{"NEW", "OLD"}) {
		t.Fatalf("conflict between the base and the new spec not found: %+v", got.Conflicts)
	}
}

func TestThinReplyIsRetried(t *testing.T) {
	var rules []string
	for i := 1; i <= 10; i++ {
		rules = append(rules, fmt.Sprintf(`{"statement_id":"s%d","kind":"informal","rationale":"not checkable"}`, i))
	}
	thin := `{"rules":[` + strings.Join(rules, ",") + `]}`
	if !thinReply(thin, 10) {
		t.Fatal("ten two-word informal readings should look cut short")
	}
	if thinReply(thin, 5) {
		t.Fatal("a small request is never judged cut short")
	}
	rules[0] = `{"statement_id":"s1","kind":"informal","rationale":"a user interface layout judgment no check decides"}`
	for i := 1; i < 6; i++ {
		rules[i] = fmt.Sprintf(`{"statement_id":"s%d","kind":"invariant","assert":"order.paid"}`, i+1)
	}
	if thinReply(`{"rules":[`+strings.Join(rules, ",")+`]}`, 10) {
		t.Fatal("mostly translated reply judged cut short")
	}
}

// recorded reads a saved formalize state named by an environment variable;
// the recordings live in the research repository, so without it the test
// is skipped.
func recorded(t *testing.T, env string) ([]byte, error) {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skip(env + " not set: optional recorded state absent")
	}
	return os.ReadFile(path)
}

// On a backend that caches, a wave of several requests is preceded by one
// that asks for nothing, with the same system prompt, holding the shared
// vocabulary: the wave's requests then read it from the cache.
func TestCompactWarmsTheCacheBeforeAWave(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts = append(stmts[:1], translate.Statement{ID: "S2", Source: "b.md#L1", Prose: "Orders MUST be paid before shipping."})
	opt.Parallel = 2
	var mu sync.Mutex
	var systems []string
	warm := 0
	m := cachingFunc(func(req llm.Request) (*llm.Reply, error) {
		mu.Lock()
		defer mu.Unlock()
		systems = append(systems, req.System)
		if req.Turns[0].Text == warmCompact {
			warm++
			if len(systems) != 1 {
				t.Error("the warm-up must go before the wave")
			}
			return &llm.Reply{JSON: `{"entities":[],"extensions":[],"operations":[],"rules":[]}`}, nil
		}
		if strings.Contains(req.Turns[0].Text, "# Shared vocabulary") {
			t.Error("the shared vocabulary must be in the system prompt, which caches")
		}
		return compactFixtureReply(true, compactRule("s1", "order.paid = true")), nil
	})
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil {
		t.Fatal(err)
	}
	if warm != 1 || len(systems) < 3 || systems[0] != systems[1] || systems[1] != systems[2] {
		t.Errorf("%d warm-up(s) over %d requests; the wave must share the warm-up's system prompt", warm, len(systems))
	}
}

type cachingFunc func(llm.Request) (*llm.Reply, error)

func (f cachingFunc) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) { return f(req) }
func (f cachingFunc) Name() string                                                    { return "caching" }
func (f cachingFunc) CachesPrompts() bool                                             { return true }

// Terms a wave adds go in the next wave's request, so its system prompt
// stays the one the first wave cached.
func TestCompactKeepsTheCachedPromptWhenAWaveAddsTerms(t *testing.T) {
	dir, stmts, opt := compactFixture(t)
	stmts = append(stmts[:1], translate.Statement{ID: "S2", Source: "b.md#L1", Prose: "Orders MUST be paid before shipping."})
	opt.Parallel = 1
	var systems, requests []string
	m := cachingFunc(func(req llm.Request) (*llm.Reply, error) {
		systems = append(systems, req.System)
		requests = append(requests, req.Turns[0].Text)
		if len(systems) == 1 {
			return compactFixtureReply(true, compactRule("s1", "order.paid = true")), nil
		}
		return compactFixtureReply(false, compactRule("s1", "order.paid = true")), nil
	})
	if _, err := Run(context.Background(), m, stmts, dir, opt); err != nil {
		t.Fatal(err)
	}
	if len(systems) < 2 || systems[0] != systems[1] {
		t.Fatalf("%d requests; the second wave must keep the first wave's system prompt", len(systems))
	}
	if added, _, _ := strings.Cut(requests[1], "# Statements"); !strings.Contains(added, "# Vocabulary added in this run") || !strings.Contains(added, "paid: bool") {
		t.Errorf("the second request must show the term the first added:\n%s", requests[1])
	}
	if strings.Contains(systems[1], "paid: bool") {
		t.Error("a term this run added is in the system prompt")
	}
}

func TestVocabularyDeltaKeepsHeadingsOfChangedEntries(t *testing.T) {
	base := "Entity Order (context shop)\n  total: int [set by the environment]\n\nOperations:\n  Order.place (default allow; writes Order.total)\n"
	cur := "Entity Order (context shop)\n  paid: bool [written by Order.pay]\n  total: int [set by the environment]\nEntity Refund (context shop)\n  amount: int [set by the environment]\n\nOperations:\n  Order.pay (default allow; writes Order.paid)\n  Order.place (default allow; writes Order.total)\n"
	want := "Entity Order (context shop)\n  paid: bool [written by Order.pay]\nEntity Refund (context shop)\n  amount: int [set by the environment]\nOperations:\n  Order.pay (default allow; writes Order.paid)\n"
	if got := vocabularyDelta(base, cur); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := vocabularyDelta(cur, cur); got != "" {
		t.Errorf("an unchanged vocabulary adds %q", got)
	}
}

// A reply groups rules under their statement, which names its overrides
// once; each rule then reads as if it named them itself.
func TestDecodeCompactReadsRulesGroupedByStatement(t *testing.T) {
	aliases := map[string]string{"s1": "orders.md#paid", "s2": "orders.md#tone"}
	reply := `{"entities":[],"extensions":[],"operations":[],"statements":[
		{"statement_id":"s1","overrides":["FR-005"],"rules":[{"kind":"invariant","subject":"","on":"","when":"","assert":"order.paid = true"},{"kind":"informal","rationale":"a judgment"}]},
		{"statement_id":"s2","overrides":[],"rules":[{"kind":"informal","rationale":"tone of voice"}]}]}`
	out, err := decodeCompact(reply, aliases)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rules) != 3 || out.Rules[0].StatementID != "orders.md#paid" || out.Rules[2].StatementID != "orders.md#tone" {
		t.Fatalf("rules: %+v", out.Rules)
	}
	if !slices.Equal(out.Rules[0].Overrides, []string{"FR-005"}) || out.Rules[1].Overrides != nil {
		t.Errorf("overrides go to the rules that take them: %v, %v", out.Rules[0].Overrides, out.Rules[1].Overrides)
	}
	if _, err := decodeCompact(`{"entities":[],"extensions":[],"operations":[],"statements":[{"statement_id":"s1","overrides":[],"rules":[{"statement_id":"s2","kind":"informal","rationale":"x"}]}]}`, aliases); err == nil {
		t.Error("a rule that names another statement was accepted")
	}
	schema, err := compactSchema()
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal([]byte(schema), &top); err != nil || !slices.Contains(top.Required, "statements") || slices.Contains(top.Required, "rules") {
		t.Errorf("the reply must require statements, not a flat rules list: %v %v", top.Required, err)
	}
}
