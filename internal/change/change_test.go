package change

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
	"github.com/provasign/stele/internal/translate"
	"github.com/provasign/stele/internal/vocab"
)

const vocabYAML = `context: res
formatVersion: 1
---
extend: platform.Request
context: res
attributes:
  kind: { type: enum, values: [normal, erasure] }
---
entity: Resource
context: res
attributes:
  holds: { type: int, range: [0, 5] }
---
operation: Resource.delete
context: res
default: allow
writes: []
`

const guardProse = "Requirement: Delete guard\nThe system MUST NOT delete a resource with holds."

const truth = "# Resources\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds.\n"

// fake answers compact translation requests with fixed readings, by a
// word of the statement's text; any other request gets an empty reply.
type fake struct {
	readings map[string]reading
	calls    *[]string
	// uncovered, when set, is a clause the coverage check finds no rule for.
	uncovered string
	// nothingCheckable: the coverage check finds no clause a rule could
	// check, as for a one-off task.
	nothingCheckable bool
	// coverDown: the coverage check's backend fails.
	coverDown bool
}

type reading struct {
	rules      []map[string]any
	extensions []map[string]any
}

func (f fake) Name() string { return "fake" }
func (f fake) Complete(_ context.Context, req llm.Request) (*llm.Reply, error) {
	text := req.Turns[len(req.Turns)-1].Text
	if !strings.Contains(text, "statement_id: s1") {
		return &llm.Reply{JSON: "{}"}, nil
	}
	if strings.Contains(text, "list its clauses, say which rules cover each") && f.coverDown {
		return nil, fmt.Errorf("coverage backend unavailable")
	}
	if strings.Contains(text, "list its clauses, say which rules cover each") {
		// The clause coverage check: every statement is one covered clause.
		var sts []map[string]any
		for _, block := range strings.Split(text, "statement_id: ")[1:] {
			alias, _, _ := strings.Cut(block, "\n")
			clauses := []map[string]any{{"text": "all", "status": "covered", "rules": []string{"r1"}}}
			if f.nothingCheckable {
				clauses = []map[string]any{{"text": "all", "status": "not_checkable", "rules": []string{}}}
			}
			if f.uncovered != "" {
				clauses = append(clauses, map[string]any{"text": f.uncovered, "status": "uncovered", "rules": []string{}})
			}
			sts = append(sts, map[string]any{"statement_id": alias, "clauses": clauses, "broader": []any{}})
		}
		js, _ := json.Marshal(map[string]any{"statements": sts})
		return &llm.Reply{JSON: string(js)}, nil
	}
	if f.calls != nil {
		*f.calls = append(*f.calls, text)
	}
	rules, exts := []map[string]any{}, []map[string]any{}
	for _, block := range strings.Split(text, "statement_id: ")[1:] {
		alias, _, _ := strings.Cut(block, "\n")
		for word, r := range f.readings {
			if !strings.Contains(block, word) {
				continue
			}
			for _, x := range r.rules {
				y := map[string]any{"statement_id": alias, "kind": "invariant", "subject": "", "on": "", "when": "", "assert": "", "overrides": []string{}}
				if x["kind"] == "permission" {
					y = map[string]any{"statement_id": alias, "kind": "permission", "operation": "", "effect": "", "when": "", "overrides": []string{}}
				}
				if x["kind"] == "informal" {
					y = map[string]any{"statement_id": alias}
				}
				for k, v := range x {
					y[k] = v
				}
				rules = append(rules, y)
			}
			exts = append(exts, r.extensions...)
		}
	}
	js, _ := json.Marshal(map[string]any{"entities": []any{}, "extensions": exts, "operations": []any{}, "rules": rules})
	return &llm.Reply{JSON: string(js)}, nil
}

func one(word string, rule map[string]any) fake {
	return fake{readings: map[string]reading{word: {rules: []map[string]any{rule}}}}
}

func setup(t *testing.T, delta string) (repo, registry string) {
	t.Helper()
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	repo, registry = t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, "openspec/specs/res/spec.md"), truth)
	write(filepath.Join(repo, "openspec/changes/c1/specs/res/spec.md"), delta)
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "rules.yaml"), `id: res/Delete guard
kind: permission
effect: deny
operation: Resource.delete
when: self.holds > 0
source:
  statementId: res/Delete guard
  doc: openspec/specs/res/spec.md#L3
  textHash: `+translate.TextHash(guardProse)+`
`)
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"res/Delete guard"}, "owner"); err != nil {
		t.Fatal(err)
	}
	return repo, registry
}

func TestAddedRequirementConflicts(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n")
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(),
		Clients: []llm.Client{one("Erasure", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"})}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, ",") != "res/Erasure" || len(res.Rules["res/Erasure"]) != 1 {
		t.Fatalf("added %v, rules %v, unchecked %v", res.Added, res.Rules, res.Unchecked)
	}
	g := res.Gate
	if g.Accepted || len(g.Errors) > 0 || g.Check == nil || len(g.Check.Conflicts) != 1 {
		t.Fatalf("gate = %+v", g)
	}
	if got := strings.Join(g.Check.Conflicts[0].Rules, ","); !strings.Contains(got, "res/Delete guard") || !strings.Contains(got, "res/Erasure") {
		t.Fatalf("conflict = %s", got)
	}
	if strings.Join(g.Unapproved, ",") != "res/Erasure" {
		t.Fatalf("unapproved = %v", g.Unapproved)
	}
}

func TestModifiedRequirementReplacesRule(t *testing.T) {
	repo, registry := setup(t, "## MODIFIED Requirements\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds, unless the request is an erasure.\n")
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(),
		Clients: []llm.Client{one("unless", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0 and request.kind != erasure"})}})
	if err != nil {
		t.Fatal(err)
	}
	g := res.Gate
	if strings.Join(res.Replaced, ",") != "res/Delete guard" || len(res.Retired) != 0 {
		t.Fatalf("replaced %v retired %v", res.Replaced, res.Retired)
	}
	if len(g.Errors) > 0 || g.Check == nil || len(g.Check.Conflicts) > 0 {
		t.Fatalf("gate = %+v", g)
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		t.Fatal(err)
	}
	if x := head.Rule("res/Delete guard"); x.Version != 2 || x.Approved() {
		t.Fatalf("head rule = %+v", x)
	}
	if strings.Join(g.Unapproved, ",") != "res/Delete guard" {
		t.Fatalf("unapproved = %v", g.Unapproved)
	}
}

func TestRemovedRequirementRetiresRule(t *testing.T) {
	repo, registry := setup(t, "## REMOVED Requirements\n\n### Requirement: Delete guard\n**Reason**: holds are gone.\n")
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Retired, ",") != "res/Delete guard" || !res.Gate.Accepted {
		t.Fatalf("retired %v gate %+v", res.Retired, res.Gate)
	}
	// The approval applies the retirement, so a removal-only change asks
	// for it (a code review, 2026-11-03).
	if len(res.Questions) != 1 || res.Questions[0].Kind != "approve" || !strings.Contains(res.Questions[0].Text, "retires 1 approved rule(s) (res/Delete guard)") {
		t.Fatalf("questions %+v", res.Questions)
	}
}

func TestNewTermReuseAndAnswer(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Locks\nThe system MUST NOT delete a locked resource.\n")
	out := t.TempDir()
	var calls []string
	locked := fake{calls: &calls, readings: map[string]reading{"locked": {
		rules:      []map[string]any{{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.locked"}},
		extensions: []map[string]any{{"name": "Resource", "statement_ids": []string{"s1"}, "attributes": []map[string]any{{"name": "locked", "type": "bool", "values": []string{}, "min": 0, "max": 0}}, "relations": []any{}}},
	}}}
	opt := Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{locked}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.NewTerms, ",") != "Resource.locked" || len(res.Rules["res/Locks"]) != 1 || !res.Gate.Accepted {
		t.Fatalf("new terms %v, rules %v, unchecked %v, gate %+v", res.NewTerms, res.Rules, res.Unchecked, res.Gate)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "approve" || !res.Questions[0].AskOwner {
		t.Fatalf("questions = %+v", res.Questions)
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil || head.Entities["Resource"].Attrs["locked"] == nil {
		t.Fatalf("head lacks the new term: %v", err)
	}

	// The same inputs again: the translation is reused, nothing is asked of the model.
	before := len(calls)
	res, err = runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != before || strings.Join(res.Reused, ",") != "res/Locks" || strings.Join(res.NewTerms, ",") != "Resource.locked" {
		t.Fatalf("calls %d -> %d, reused %v, terms %v", before, len(calls), res.Reused, res.NewTerms)
	}

	// The owner says the new term is an existing one: the requirement is read
	// again with that note, and the term is gone.
	prose := res.Statements["res/Locks"]
	if err := SaveAnswer(out, Answer{Question: "same-x", Kind: "same_term", Choice: "same", Term: "Resource.locked", Existing: "Resource.holds",
		Statements: map[string]string{"res/Locks": translate.TextHash(prose)}}); err != nil {
		t.Fatal(err)
	}
	locked.readings["Resource.holds"] = reading{rules: []map[string]any{{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0"}}}
	delete(locked.readings, "locked")
	res, err = runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != before+1 || !strings.Contains(calls[len(calls)-1], "use Resource.holds and do not propose Resource.locked") {
		t.Fatalf("the requirement was not read again with the answer: %v", calls[before:])
	}
	if len(res.NewTerms) != 0 || len(res.Reused) != 0 || len(res.Rules["res/Locks"]) != 1 {
		t.Fatalf("terms %v reused %v rules %v unchecked %v", res.NewTerms, res.Reused, res.Rules, res.Unchecked)
	}
}

func TestQuestionsForConflicts(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n")
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(),
		Clients: []llm.Client{one("Erasure", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"})}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) != 1 {
		t.Fatalf("questions = %+v", res.Questions)
	}
	q := res.Questions[0]
	var choices []string
	for _, o := range q.Options {
		choices = append(choices, o.Choice)
	}
	if q.Kind != "conflict" || !q.AskOwner || len(q.Requirements) != 2 || strings.Join(q.Statements, ",") != "res/Erasure" ||
		strings.Join(choices, ",") != "change,update_old,exception,impossible,misread" || !strings.Contains(q.Text, `"Delete guard" (current spec)`) {
		t.Fatalf("question = %+v", q)
	}
}

func TestSelfConflictIsReadAgainFirst(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Twice\nErasure requests are special.\n")
	out := t.TempDir()
	twice := fake{readings: map[string]reading{"special": {rules: []map[string]any{
		{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure and self.holds = 0"},
		{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "request.kind = erasure"},
	}}}}
	opt := Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{twice}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "self_conflict" || res.Questions[0].AskOwner {
		t.Fatalf("questions = %+v", res.Questions)
	}
	q := res.Questions[0]
	if err := SaveAnswer(out, Answer{Question: q.ID, Kind: q.Kind, Choice: "retranslate", Note: q.Detail,
		Statements: map[string]string{"res/Twice": translate.TextHash(res.Statements["res/Twice"])}}); err != nil {
		t.Fatal(err)
	}
	res, err = runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "self_conflict" || !res.Questions[0].AskOwner {
		t.Fatalf("after one re-read the owner is asked: %+v", res.Questions)
	}
}

func TestCreatingStepIsAskedOfTheAgent(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	repo, registry, out := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keyProse := "Requirement: Backup key\nA backup's key MUST name the cluster's tenant."
	write(filepath.Join(repo, "openspec/specs/res/spec.md"), "# Clusters\n\n### Requirement: Backup key\nA backup's key MUST name the cluster's tenant.\n")
	write(filepath.Join(repo, "openspec/changes/c1/specs/res/spec.md"), "## ADDED Requirements\n\n### Requirement: Default tenant\nInit MUST put a cluster in the default tenant.\n")
	write(filepath.Join(registry, "vocabulary.yaml"), `context: res
formatVersion: 1
---
entity: Cluster
context: res
attributes:
  tenant: { type: enum, values: [a, b, default] }
  backupTenant: { type: enum, values: [a, b, default] }
  backedUp: { type: bool }
---
operation: Cluster.init
context: res
default: allow
writes: [Cluster.tenant]
`)
	write(filepath.Join(registry, "rules.yaml"), `id: res/Backup key
kind: invariant
subject: Cluster
when: self.backedUp
assert: self.backupTenant = self.tenant
source:
  statementId: res/Backup key
  doc: openspec/specs/res/spec.md#L3
  textHash: `+translate.TextHash(keyProse)+`
`)
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"res/Backup key"}, "owner"); err != nil {
		t.Fatal(err)
	}
	opt := Options{Repo: repo, Change: "c1", Registry: registry, Out: out,
		Clients: []llm.Client{one("default tenant", map[string]any{"kind": "invariant", "on": "Cluster.init", "subject": "Cluster", "assert": "self.tenant = default"})}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "creates" || res.Questions[0].AskOwner {
		t.Fatalf("questions = %+v", res.Questions)
	}
	q := res.Questions[0]
	if err := SaveAnswer(out, Answer{Question: q.ID, Kind: q.Kind, Choice: "creates", Term: q.Term, Existing: q.Existing}); err != nil {
		t.Fatal(err)
	}
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if !res.Gate.Accepted || len(res.Questions) != 1 || res.Questions[0].Kind != "approve" {
		t.Fatalf("after the answer: gate %+v questions %+v", res.Gate.Check, res.Questions)
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil || strings.Join(head.Entities["Cluster"].CreatedBy, ",") != "Cluster.init" {
		t.Fatalf("head: %v", err)
	}
	// An answer about a step and an entity a later reading no longer has
	// is left out, and the change's registry still loads.
	if err := SaveAnswer(out, Answer{Question: "creates-gone", Kind: "creates", Choice: "creates", Term: "Plot.add", Existing: "Plot"}); err != nil {
		t.Fatal(err)
	}
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatalf("a stale answer broke the change: %v", err)
	}
	if _, err := ir.LoadDir(res.Head); err != nil {
		t.Fatalf("head with a stale answer: %v", err)
	}
}

// TestReportedWriteIsCorrected: an approved listing recorded as writing the
// tool's name, which its rule only shows, kept the write after inference
// learned better, and nothing corrected it (ISSUES 90). A change that meets
// the step asks the agent; "keeps" takes the write back in the change's
// registry, and the approval says so instead of a meaning-change question.
func TestReportedWriteIsCorrected(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
	repo, registry, out := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prose := "Requirement: List names\nThe listing MUST show each tool's name against its id."
	write(filepath.Join(repo, "openspec/specs/shed/spec.md"), "# Shed\n\n### Requirement: List names\nThe listing MUST show each tool's name against its id.\n")
	write(filepath.Join(repo, "openspec/changes/c1/specs/shed/spec.md"), "## ADDED Requirements\n\n### Requirement: List count\nThe listing MUST show how many tools there are.\n")
	write(filepath.Join(registry, "vocabulary.yaml"), `context: shed
formatVersion: 1
---
entity: Tool
context: shed
attributes:
  id: { type: int }
  name: { type: string }
---
extend: Result
context: shed
attributes:
  nameOf: { type: "map<int, string>" }
  count: { type: int }
---
operation: Tool.list
context: shed
default: allow
writes: [Tool.name]
`)
	write(filepath.Join(registry, "rules.yaml"), `id: shed/List names
kind: invariant
on: Tool.list
assert: result.nameOf[self.id] = self.name
source:
  statementId: shed/List names
  doc: openspec/specs/shed/spec.md#L3
  textHash: `+translate.TextHash(prose)+`
`)
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"shed/List names"}, "owner"); err != nil {
		t.Fatal(err)
	}
	opt := Options{Repo: repo, Change: "c1", Registry: registry, Out: out,
		Clients: []llm.Client{one("how many", map[string]any{"kind": "invariant", "on": "Tool.list", "subject": "Tool", "assert": "result.count >= 0"})}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) == 0 || res.Questions[0].Kind != "reported_write" || res.Questions[0].AskOwner || res.Questions[0].Existing != "Tool.name" {
		t.Fatalf("questions = %+v", res.Questions)
	}
	q := res.Questions[0]
	if err := SaveAnswer(out, Answer{Question: q.ID, Kind: q.Kind, Choice: "keeps", By: "agent", Term: q.Term, Existing: q.Existing}); err != nil {
		t.Fatal(err)
	}
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "approve" || !strings.Contains(res.Questions[0].Text, "tool: list keeps tool: name") {
		t.Fatalf("after the answer: gate %+v questions %+v", res.Gate.Check, res.Questions)
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		t.Fatal(err)
	}
	if ws := head.Operations["Tool.list"].Writes; len(ws) != 0 {
		t.Errorf("the listing still writes %v", ws)
	}
}

func TestNotesAccumulate(t *testing.T) {
	dir := t.TempDir()
	stmt := map[string]string{"s": "h"}
	for _, a := range []Answer{
		{Question: "self-1", Kind: "self_conflict", Choice: "misread", Note: "owner", Statements: stmt},
		{Question: "self-1", Kind: "self_conflict", Choice: "retranslate", Note: "rules", Statements: stmt},
		{Question: "same-1", Kind: "same_term", Choice: "same"},
		{Question: "same-1", Kind: "same_term", Choice: "different"},
	} {
		if err := SaveAnswer(dir, a); err != nil {
			t.Fatal(err)
		}
	}
	as, err := LoadAnswers(dir)
	if err != nil || len(as) != 3 || as[2].Choice != "different" {
		t.Fatalf("answers = %+v, %v", as, err)
	}
}

// A rule about the state after a step the registry declares makes the step
// write what it asserts, as formalize does for the steps it proposes.
func TestRuleOnExistingStepInfersWrites(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Release\nDeleting a resource MUST release its holds.\n")
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(),
		Clients: []llm.Client{one("release its holds", map[string]any{"kind": "invariant", "on": "Resource.delete", "subject": "Resource", "assert": "self.holds = 0 and old(self.holds) >= 0"})}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.InferredWrites["Resource.delete"], ","); got != "Resource.holds" {
		t.Fatalf("inferred writes = %v", res.InferredWrites)
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		t.Fatal(err)
	}
	if op := head.Operations["Resource.delete"]; len(op.Writes) != 1 || op.Writes[0].Name != "holds" {
		t.Fatalf("head writes = %v", op.Writes)
	}
	for _, q := range res.Questions {
		if q.Kind == "meaning_change" {
			t.Fatalf("the inferred write was asked about: %+v", q)
		}
	}
}

// A Spec Kit feature branch is the difference between the specs at the
// base ref and the working tree.
func TestSpecKitBranch(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(filepath.Join(repo, "specs/001-res/spec.md"), "# Resources\n\n- **FR-001**: The system MUST NOT delete a resource with holds.\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	write(filepath.Join(repo, "specs/002-erasure/spec.md"), "# Erasure\n\n- **FR-001**: Erasure requests MUST always delete the resource.\n")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "rules.yaml"), "id: 001/FR-001\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: 001/FR-001\n  doc: specs/001-res/spec.md#L3\n  textHash: "+translate.TextHash("FR-001: The system MUST NOT delete a resource with holds.")+"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"001/FR-001"}, "owner"); err != nil {
		t.Fatal(err)
	}
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "erasure", Registry: registry, Out: t.TempDir(), Base: "main", Format: "speckit",
		Clients: []llm.Client{one("Erasure requests", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"})}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, ",") != "002/FR-001" || len(res.Modified)+len(res.Removed) != 0 {
		t.Fatalf("added %v modified %v removed %v", res.Added, res.Modified, res.Removed)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "conflict" || !strings.Contains(res.Questions[0].Text, `"FR-001: The system MUST NOT delete a resource with holds." (current spec)`) {
		t.Fatalf("questions = %+v", res.Questions)
	}
}

// Requests declared with `shale intent` are the specs: a request on the
// branch is checked against the approved earlier ones, and retiring the
// earlier one removes its rule.
func TestShaleBranch(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(filepath.Join(repo, ".shale/s1.yaml"), "id: s1\nintent:\n    title: Protect held resources\n    body: The system must not delete a resource with holds.\n    declared_at: 2026-10-01T10:00:00Z\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	// The agent declares the next request; the session is still open.
	write(filepath.Join(repo, ".shale/local/s2.jsonl"), `{"kind":"intent","at":"2026-10-02T10:00:00Z","title":"Honour erasure requests","body":"Erasure requests always delete the resource."}`+"\n")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "rules.yaml"), "id: intent/s1\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: intent/s1\n  doc: .shale/s1.yaml\n  textHash: "+translate.TextHash("Protect held resources\nThe system must not delete a resource with holds.")+"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"intent/s1"}, "owner"); err != nil {
		t.Fatal(err)
	}
	client := one("Erasure requests", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"})
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "erasure", Registry: registry, Out: t.TempDir(), Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, ",") != "intent/s2" || len(res.Modified)+len(res.Removed) != 0 {
		t.Fatalf("added %v modified %v removed %v", res.Added, res.Modified, res.Removed)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "conflict" || !strings.Contains(res.Questions[0].Text, `"Honour erasure requests"`) || !strings.Contains(res.Questions[0].Text, `"Protect held resources"`) {
		t.Fatalf("questions = %+v", res.Questions)
	}
	labels := map[string]string{}
	for _, o := range res.Questions[0].Options {
		labels[o.Choice] = o.Label
	}
	if labels["update_old"] != "The earlier request is out of date: retire it" || labels["change"] != "Change the new request" {
		t.Fatalf("options = %+v", res.Questions[0].Options)
	}

	// The owner retires the earlier request: its rule goes, and the new
	// request stands alone.
	if err := importer.RetireIntents(repo, []importer.RetiredIntent{{ID: "intent/s1", By: "owner", For: "intent/s2"}}); err != nil {
		t.Fatal(err)
	}
	res, err = runKept(context.Background(), Options{Repo: repo, Change: "erasure", Registry: registry, Out: t.TempDir(), Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != "intent/s1" || strings.Join(res.Retired, ",") != "intent/s1" {
		t.Fatalf("removed %v retired %v", res.Removed, res.Retired)
	}
	for _, q := range res.Questions {
		if q.Kind == "conflict" {
			t.Fatalf("still a conflict after retiring: %+v", q)
		}
	}
}

// The owner asks for something new in the same conversation, after the
// first request was approved on this branch: the new request is checked
// against the first, and retiring the first removes its rule.
func TestShaleSameSessionOnBranch(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(filepath.Join(repo, ".shale/config.yaml"), "privacy: redacted\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	// On the branch: the first request, approved, and its session finalized.
	first := "Protect held resources\nThe system must not delete a resource with holds."
	write(filepath.Join(repo, ".shale/s1.yaml"), "id: s1\nfinalized_at: 2026-10-01T10:30:00Z\nintent:\n    title: Protect held resources\n    body: The system must not delete a resource with holds.\n    declared_at: 2026-10-01T10:00:00Z\n")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "change-feature.yaml"), "id: intent/s1\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: intent/s1\n  doc: .shale/s1.yaml\n  textHash: "+translate.TextHash(first)+"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"intent/s1"}, "owner"); err != nil {
		t.Fatal(err)
	}
	// The same conversation, a new request.
	write(filepath.Join(repo, ".shale/local/s1.jsonl"), `{"kind":"intent","at":"2026-10-01T11:00:00Z","title":"Honour erasure requests","body":"Erasure requests always delete the resource."}`+"\n")
	client := one("Erasure requests", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"})
	opt := Options{Repo: repo, Change: "feature-2", Registry: registry, Out: t.TempDir(), Base: "main", Format: "shale", Clients: []llm.Client{client}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, ",") != "intent/s1-c2" || len(res.Modified)+len(res.Removed) != 0 {
		t.Fatalf("added %v modified %v removed %v; want the new request alone", res.Added, res.Modified, res.Removed)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "conflict" {
		t.Fatalf("questions = %+v; want the conflict with the first request", res.Questions)
	}

	if err := importer.RetireIntents(repo, []importer.RetiredIntent{{ID: "intent/s1", By: "owner", For: "intent/s1-c2"}}); err != nil {
		t.Fatal(err)
	}
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Removed, ",") != "intent/s1" || strings.Join(res.Retired, ",") != "intent/s1" {
		t.Fatalf("removed %v retired %v; want the first request and its rule gone", res.Removed, res.Retired)
	}
	for _, q := range res.Questions {
		if q.Kind == "conflict" {
			t.Fatalf("still a conflict after retiring: %+v", q)
		}
	}
}

// A conflict whose one condition lies inside the other's recommends
// reading the narrower requirement as an exception (ISSUES 56).
func TestNarrowerRequirementIsRecommendedAsException(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST delete a resource even with holds.\n")
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(),
		Clients: []llm.Client{one("Erasure", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "self.holds > 0 and request.kind = erasure"})}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Questions) != 1 {
		t.Fatalf("questions = %+v", res.Questions)
	}
	q := res.Questions[0]
	if q.Recommended != "exception" || q.Options[0].Choice != "exception" || !strings.Contains(q.Why, `"Erasure" applies only in situations "Delete guard" also covers`) {
		t.Fatalf("question = %+v", q)
	}
}

func TestTextPrecedence(t *testing.T) {
	prose := map[string]string{
		"010/FR-003b": "FR-003b: System MUST respect --color (always, never, auto)",
		"010/FR-008a": "FR-008a: System MUST disable colour with --raw, regardless of `--color` (FR-003b)",
		"010/FR-002":  "FR-002: Output MUST go in colour unless the terminal cannot show it (FR-005b)",
		"010/FR-005b": "FR-005b: fall back when the terminal has no colour",
		// A denied precedence names no override (a code review, 2026-11-04);
		// a qualifying one after a negated verb still does.
		"010/FR-009": "FR-009: The pager MUST NOT override `--color` (FR-003b)",
		"010/FR-010": "FR-010: Paging doesn't take precedence over FR-005b",
		"010/FR-011": "FR-011: The pager MUST NOT stop regardless of FR-005b",
	}
	cands := []translate.Candidate{{StatementID: "010/FR-008a", Outcome: "rule"}, {StatementID: "010/FR-002", Outcome: "rule"},
		{StatementID: "010/FR-009", Outcome: "rule"}, {StatementID: "010/FR-010", Outcome: "rule"}, {StatementID: "010/FR-011", Outcome: "rule"}}
	textPrecedence(cands, prose)
	got := make([]string, len(cands))
	for i, c := range cands {
		got[i] = strings.Join(c.Overrides, ",")
	}
	if want := []string{"010/FR-003b", "", "", "", "010/FR-005b"}; !slices.Equal(got, want) {
		t.Fatalf("overrides = %q, want %q", got, want)
	}
}

// Conflicts one changed requirement has with several others in one step
// are one question (ISSUES 60).
func TestConflictsOfOneRequirementAreGrouped(t *testing.T) {
	head := &ir.Registry{}
	req := func(id string) Requirement { return Requirement{ID: id, Title: id, Where: "ADDED in this change"} }
	mk := func(other string) Question {
		return Question{ID: other, Kind: "conflict", AskOwner: true, Statements: []string{"a/FR-002", other}, unit: "Pager.launch",
			stmts: []string{"a/FR-002", other}, Options: []Option{{Choice: "change"}, {Choice: "exception"}}}
	}
	qs := groupConflicts([]Question{{Kind: "creates"}, mk("a/FR-004e"), mk("a/FR-005b")}, head, req)
	if len(qs) != 2 || qs[0].Kind != "creates" || len(qs[1].Requirements) != 3 || !strings.Contains(qs[1].Text, `"a/FR-004e" (added in this change) and "a/FR-005b"`) || len(qs[1].Options) != 2 {
		t.Fatalf("questions = %+v", qs)
	}
}

// A base ref without the registry the branch has committed is refused: the
// change would be measured against the wrong branch (ISSUES 63).
func TestBaseWithoutRegistryIsRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "master")
	git("commit", "-q", "--allow-empty", "-m", "upstream")
	reg := filepath.Join(repo, ".stele", "registry")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reg, "vocabulary.yaml"), []byte(vocabYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	opt := Options{Repo: repo, Registry: reg, Base: "master"}
	if err := baseHoldsRegistry(opt); err != nil {
		t.Fatalf("an uncommitted registry is not checked: %v", err)
	}
	git("checkout", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "registry")
	start, _ := exec.Command("git", "-C", repo, "rev-parse", "--short", "HEAD").Output()
	if err := baseHoldsRegistry(opt); err == nil || !strings.Contains(err.Error(), "no approved registry at .stele/registry") || !strings.Contains(err.Error(), "--base "+strings.TrimSpace(string(start))) {
		t.Fatalf("base master: %v; want the commit that added the registry named", err)
	}
	opt.Base = "main"
	if err := baseHoldsRegistry(opt); err != nil {
		t.Fatalf("base main: %v", err)
	}
}

const renameDelta = "## RENAMED Requirements\n- FROM: `### Requirement: Delete guard`\n- TO: `### Requirement: Hold guard`\n"

// A renamed requirement keeps its rule: no retirement, no new translation.
func TestRenamedRequirementKeepsRule(t *testing.T) {
	repo, registry := setup(t, renameDelta)
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(), Clients: []llm.Client{fake{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Retired) != 0 || len(res.Added) != 0 || len(res.Replaced) != 0 || len(res.Questions) != 0 {
		t.Fatalf("retired %v added %v replaced %v questions %+v", res.Retired, res.Added, res.Replaced, res.Questions)
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		t.Fatal(err)
	}
	if x := head.Rule("res/Delete guard"); x == nil || !x.Live() || x.StatementID != "res/Hold guard" || !x.Approved() {
		t.Fatalf("head rule = %+v", x)
	}
}

// A requirement renamed and edited in one change keeps its rule's identity:
// the rule gets a new version under its old ID, not a retirement and a
// stranger.
func TestRenamedAndEditedRequirementVersionsRule(t *testing.T) {
	repo, registry := setup(t, renameDelta+"\n## MODIFIED Requirements\n\n### Requirement: Hold guard\nThe system MUST NOT delete a resource with holds, unless the request is an erasure.\n")
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(),
		Clients: []llm.Client{one("unless", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0 and request.kind != erasure"})}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Replaced, ",") != "res/Delete guard" || len(res.Retired) != 0 || len(res.Added) != 0 || strings.Join(res.Modified, ",") != "res/Hold guard" {
		t.Fatalf("replaced %v retired %v added %v modified %v", res.Replaced, res.Retired, res.Added, res.Modified)
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		t.Fatal(err)
	}
	if x := head.Rule("res/Delete guard"); x == nil || x.Version != 2 || x.StatementID != "res/Hold guard" || x.Approved() {
		t.Fatalf("head rule = %+v", x)
	}
	if strings.Join(res.Gate.Unapproved, ",") != "res/Delete guard" {
		t.Fatalf("unapproved = %v", res.Gate.Unapproved)
	}
}

// A requirement rewritten under a new title with no RENAMED record looks
// like a removal and an addition. Stele notices the similar text and asks;
// "same" makes it a rename on the next run.
func TestRewrittenRequirementWithoutRenameRecordAsks(t *testing.T) {
	repo, registry := setup(t, "## REMOVED Requirements\n\n### Requirement: Delete guard\n**Reason**: renamed.\n\n## ADDED Requirements\n\n### Requirement: Hold guard\nThe system MUST NOT delete a resource with holds, unless the request is an erasure.\n")
	out := t.TempDir()
	opt := Options{Repo: repo, Change: "c1", Registry: registry, Out: out,
		Clients: []llm.Client{one("unless", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0 and request.kind != erasure"})}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	// Until the owner answers, it is what the change says: a removal and an addition.
	if strings.Join(res.Retired, ",") != "res/Delete guard" || strings.Join(res.Added, ",") != "res/Hold guard" {
		t.Fatalf("retired %v added %v", res.Retired, res.Added)
	}
	var q *Question
	for i := range res.Questions {
		if res.Questions[i].Kind == "renamed" {
			q = &res.Questions[i]
		}
	}
	if q == nil || !q.AskOwner || q.Term != "res/Delete guard" || q.Existing != "res/Hold guard" || len(q.Options) != 2 {
		t.Fatalf("no rename question: %+v", res.Questions)
	}
	if err := SaveAnswer(out, Answer{Question: q.ID, Kind: "renamed", Choice: "same", Term: q.Term, Existing: q.Existing, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	res, err = runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Replaced, ",") != "res/Delete guard" || len(res.Retired) != 0 || len(res.Added) != 0 || strings.Join(res.Modified, ",") != "res/Hold guard" {
		t.Fatalf("after the answer: replaced %v retired %v added %v modified %v", res.Replaced, res.Retired, res.Added, res.Modified)
	}
	for _, x := range res.Questions {
		if x.Kind == "renamed" {
			t.Fatalf("rename asked again: %+v", x)
		}
	}
	head, err := ir.LoadDir(res.Head)
	if err != nil {
		t.Fatal(err)
	}
	if x := head.Rule("res/Delete guard"); x == nil || x.Version != 2 || x.StatementID != "res/Hold guard" {
		t.Fatalf("head rule = %+v", x)
	}
}

// What the coverage check finds in a reading reaches the owner with the
// approval: beside the requirement's reading, and as a reason to look first.
func TestApprovalShowsFindings(t *testing.T) {
	repo, registry := setup(t, "## MODIFIED Requirements\n\n### Requirement: Delete guard\nThe system MUST NOT delete a resource with holds, unless the request is an erasure.\n")
	f := one("unless", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0"})
	f.uncovered = "unless the request is an erasure"
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: t.TempDir(), Clients: []llm.Client{f}})
	if err != nil {
		t.Fatal(err)
	}
	var q *Question
	for i := range res.Questions {
		if res.Questions[i].Kind == "approve" {
			q = &res.Questions[i]
		}
	}
	if q == nil {
		t.Fatalf("no approval question: %+v", res.Questions)
	}
	r := q.Requirements[0]
	if len(r.Reading) != 1 || !strings.Contains(r.Reading[0], "self.holds > 0") || len(r.Findings) != 1 || !strings.Contains(r.Findings[0], `no rule checks "unless the request is an erasure"`) {
		t.Fatalf("requirement = %+v", r)
	}
	if q.Recommended != "show" || !strings.Contains(q.Text, "may say less or more") {
		t.Fatalf("question = %+v", q)
	}
	if !strings.Contains(q.Text, "agree with the approved spec") {
		t.Fatalf("question = %q; want it measured against the approved spec", q.Text)
	}
}

// The first change, into a registry with nothing approved, is not said to
// agree with an approved spec.
func TestFirstApproval(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, ".shale/config.yaml"), "privacy: redacted\n")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(filepath.Join(repo, ".shale/local/s1.jsonl"), `{"kind":"intent","at":"2026-10-01T10:00:00Z","title":"Protect held resources","body":"The system must not delete a resource with holds."}`+"\n")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	client := one("Protect held", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0"})
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "first", Registry: registry, Out: t.TempDir(), Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range res.Questions {
		if q.Kind == "approve" {
			if strings.Contains(q.Text, "approved spec") || !strings.Contains(q.Text, "nothing is approved yet") {
				t.Fatalf("question = %q", q.Text)
			}
			return
		}
	}
	t.Fatalf("no approval question: %+v", res.Questions)
}

// The owner's answer becomes a lesson in the head registry, approval
// carries it into the registry, and a later change is read with it without
// being asked; the agent's own answers, and notes that are not the owner's
// own words, teach nothing.
func TestAnswersBecomeLessons(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Locks\nThe system MUST NOT delete a locked resource.\n")
	out := t.TempDir()
	var calls []string
	f := fake{calls: &calls, readings: map[string]reading{"locked": {
		rules: []map[string]any{{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0"}},
	}}}
	opt := Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{f}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	prose := res.Statements["res/Locks"]
	pin := map[string]string{"res/Locks": translate.TextHash(prose)}
	if err := SaveAnswer(out, Answer{Question: "reread-res/Locks", Kind: "reread", Choice: "reread", Note: "a locked resource is one with holds", Quoted: true, By: "owner", Statements: pin}); err != nil {
		t.Fatal(err)
	}
	// A note the agent wrote to explain the owner's one-word answer is not
	// the owner's words, and teaches nothing.
	if err := SaveAnswer(out, Answer{Question: "free-x", Kind: "free_setting", Choice: "fixed", Note: "three, the agent's explanation", By: "owner", Statements: pin}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAnswer(out, Answer{Question: "self-x", Kind: "self_conflict", Choice: "misread", Note: "Stele's own slip", By: "agent", Statements: pin}); err != nil {
		t.Fatal(err)
	}
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if len(res.NewLessons) != 1 || !strings.Contains(res.NewLessons[0], "a locked resource is one with holds") {
		t.Fatalf("new lessons = %v, want only the owner's", res.NewLessons)
	}
	files, _ := filepath.Glob(filepath.Join(res.Head, "lessons", "*.yaml"))
	if len(files) != 1 {
		t.Fatalf("lesson files in the head = %v", files)
	}
	// Approval copies the head into the registry.
	if err := copyTree(res.Head, registry); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(registry)
	if err != nil || len(reg.Lessons) != 1 || reg.Lessons[0].Kind != ir.LessonReading {
		t.Fatalf("registry lessons = %+v, %v", reg.Lessons, err)
	}

	// A later change edits the requirement: the pinned lesson is stale and
	// not applied. A project-wide lesson still is.
	if err := os.WriteFile(filepath.Join(registry, "lessons", "L-wide.yaml"), []byte("lesson: L-wide\nkind: term\nsays: locked means held; use Resource.holds\nterms: [locked]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "openspec/changes/c2/specs/res"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "openspec/changes/c2/specs/res/spec.md"), []byte("## ADDED Requirements\n\n### Requirement: Locks\nThe system MUST NOT delete a locked resource, even for admins.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls = nil
	res, err = runKept(context.Background(), Options{Repo: repo, Change: "c2", Registry: registry, Out: t.TempDir(), Clients: []llm.Client{f}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "(L-wide): locked means held") {
		t.Fatalf("the later change was not read with the project-wide lesson:\n%s", joined)
	}
	if strings.Contains(joined, "a locked resource is one with holds") {
		t.Fatalf("a lesson pinned to the requirement's old text was applied:\n%s", joined)
	}
	if strings.Join(res.Lessons["res/Locks"], ",") != "L-wide" {
		t.Fatalf("lessons used = %v", res.Lessons)
	}
}

func TestOneLineCutsBetweenWords(t *testing.T) {
	s := "FR-001: During the summer reading program, a member MUST be able to have up to 10 books on loan at a time."
	got := oneLine(s, 80)
	if got != "FR-001: During the summer reading program, a member MUST be able to have up to..." {
		t.Errorf("oneLine = %q", got)
	}
	if got := oneLine("ein Mitglied darf höchstens fünf Bücher ausleihen", 40); !utf8.ValidString(got) || len(got) > 43 {
		t.Errorf("oneLine = %q", got)
	}
	if got := oneLine("short  text", 80); got != "short text" {
		t.Errorf("oneLine = %q", got)
	}
}

// down is a model backend that is not reachable: every call fails, as a CLI
// that is not logged in or is blocked by a sandbox does.
type down struct{ calls *int }

func (d down) Name() string { return "down" }
func (d down) Complete(context.Context, llm.Request) (*llm.Reply, error) {
	*d.calls++
	return nil, fmt.Errorf("Not logged in")
}

// A requirement the model could not translate is a failure, never a clean
// check, and it is not cached: the next check calls the model again and
// then reads it.
func TestFailedTranslationIsNotClean(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Locks\nThe system MUST NOT delete a locked resource.\n")
	calls := 0
	out := t.TempDir()
	opt := Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{down{&calls}}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 || calls == 0 || !strings.Contains(fmt.Sprint(res.Failed), "Not logged in") {
		t.Fatalf("failed = %v after %d call(s); want the requirement and the backend's error", res.Failed, calls)
	}
	for _, q := range res.Questions {
		if q.Kind == "approve" {
			t.Fatalf("approval offered for a change that was not translated: %+v", q)
		}
	}
	before := calls
	if res, err = runKept(context.Background(), opt); err != nil || len(res.Failed) != 1 || calls == before {
		t.Fatalf("rerun: failed %v, %d new call(s), %v; want the model asked again", res.Failed, calls-before, err)
	}

	// The model is back: the same check translates it.
	opt.Clients = []llm.Client{one("locked", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0"})}
	if res, err = runKept(context.Background(), opt); err != nil || len(res.Failed) != 0 || len(res.Reused) != 0 {
		t.Fatalf("after the backend recovered: failed %v, reused %v, %v", res.Failed, res.Reused, err)
	}
}

// A request with nothing to check is put to the owner: a requirement the
// product must keep, or a one-off task. Kept, it is not asked again; set
// aside, it leaves the change.
func TestShaleTaskIsSetAside(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, ".shale/config.yaml"), "privacy: redacted\n")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(filepath.Join(repo, ".shale/local/s1.jsonl"), `{"kind":"intent","at":"2026-10-01T10:00:00Z","title":"Write the quarterly review","body":"Summarise the quarter's incidents in a document."}`+"\n")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	client := fake{nothingCheckable: true, readings: map[string]reading{"quarterly": {rules: []map[string]any{{"kind": "informal", "rationale": "a one-off documentation task with nothing the system must do"}}}}}
	out := t.TempDir()
	opt := Options{Repo: repo, Change: "review", Registry: registry, Out: out, Base: "main", Format: "shale", Clients: []llm.Client{client}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	var task *Question
	for i := range res.Questions {
		if res.Questions[i].Kind == "task" {
			task = &res.Questions[i]
		}
	}
	if task == nil || !strings.Contains(task.Text, `"Write the quarterly review"`) || len(task.Options) != 2 ||
		!strings.Contains(task.Text, "nothing Stele can check") || !strings.Contains(task.Text, "done once the work is done") || !strings.Contains(task.Text, "without being able to check it") {
		t.Fatalf("questions = %+v; want the task question (added %v rules %v unchecked %v failed %v format %q)", res.Questions, res.Added, res.Rules, res.Unchecked, res.Failed, res.Format)
	}

	// Kept as a requirement: not asked again.
	if err := SaveAnswer(out, Answer{Question: task.ID, Kind: "task", Choice: "requirement"}); err != nil {
		t.Fatal(err)
	}
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	for _, q := range res.Questions {
		if q.Kind == "task" {
			t.Fatalf("asked again after the owner kept it: %+v", q)
		}
	}

	// Set aside as a task (what `stele change answer ... task` writes): it
	// leaves the change.
	if err := importer.RetireIntents(repo, []importer.RetiredIntent{{ID: "intent/s1", By: "owner", Why: "not a requirement"}}); err != nil {
		t.Fatal(err)
	}
	if res, err = runKept(context.Background(), opt); err != nil || len(res.Added) != 0 {
		t.Fatalf("after setting it aside: added %v, %v", res.Added, err)
	}
}

// A reading of nothing checkable, where the coverage judge finds a clause a
// rule can check, is a translation the model skipped: it fails, to be
// retried, instead of asking whether the request is a one-off task. Read so
// again, it is what the model makes of the text: the change goes on.
func TestSkippedTranslationFails(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, ".shale/config.yaml"), "privacy: redacted\n")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(filepath.Join(repo, ".shale/local/s1.jsonl"), `{"kind":"intent","at":"2026-10-01T10:00:00Z","title":"List saved resources","body":"Listing MUST show every saved resource."}`+"\n")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	client := fake{uncovered: "Listing MUST show every saved resource", readings: map[string]reading{"saved": {rules: []map[string]any{{"kind": "informal", "rationale": "The requested rule translation is not provided in this response."}}}}}
	out := t.TempDir()
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "list", Registry: registry, Out: out, Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Failed["intent/s1"], "the model gave no rules") {
		t.Fatalf("failed = %v; want the skipped translation", res.Failed)
	}
	for _, q := range res.Questions {
		if q.Kind == "task" || q.Kind == "approve" {
			t.Fatalf("asked %s for a translation that did not happen: %+v", q.Kind, q)
		}
	}

	// Read with no rules again, it is unchecked: the owner sees each
	// clause no rule checks and can approve.
	res, err = runKept(context.Background(), Options{Repo: repo, Change: "list", Registry: registry, Out: out, Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed again: %v", res.Failed)
	}
	if res.Unchecked["intent/s1"] == "" {
		t.Errorf("unchecked = %v; want the requirement", res.Unchecked)
	}
	if len(res.Questions) != 1 || res.Questions[0].Kind != "task" {
		t.Errorf("questions %+v; want the owner asked whether it is a requirement", res.Questions)
	}
}

// On the branch that is the base (trunk-based work on main), a request
// committed before the owner approved it is still new, and an approved
// request whose evidence is gone is removed with its rules, so the commit
// it blocked has a question to answer.
func TestShaleMeasuredAgainstTheRegistry(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write(filepath.Join(repo, ".shale/s2.yaml"), "id: s2\nintent:\n    title: Honour erasure requests\n    body: Erasure requests always delete the resource.\n    declared_at: 2026-10-02T10:00:00Z\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "committed before it was checked")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "rules.yaml"), "id: intent/s1\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: intent/s1\n  doc: .shale/s1.yaml\n  prose: Protect held resources\n  textHash: "+translate.TextHash("Protect held resources")+"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"intent/s1"}, "owner"); err != nil {
		t.Fatal(err)
	}
	client := one("Erasure requests", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"})
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "main", Registry: registry, Out: t.TempDir(), Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Added, ",") != "intent/s2" || strings.Join(res.Removed, ",") != "intent/s1" || len(res.Modified) != 0 {
		t.Fatalf("added %v modified %v removed %v; want s2 added and s1 removed", res.Added, res.Modified, res.Removed)
	}
}

// A conflict that rests on a world fact approved with an earlier change:
// the owner says the assumption is wrong, and the next check retires it, so
// the conflict does not come back however often it is answered.
func TestWrongApprovedFactIsRetired(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry, out := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, ".shale/s2.yaml"), "id: s2\nintent:\n    title: Held resources exist\n    body: Every resource keeps a hold.\n")
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "world.yaml"), "id: world-old-1\nkind: invariant\nassert: resource.holds = 0\nsource:\n  inferredBy: stele change\n  prose: \"World fact: resources have no holds.\"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"world-old-1"}, "owner"); err != nil {
		t.Fatal(err)
	}
	client := one("Held resources", map[string]any{"kind": "invariant", "assert": "resource.holds > 0"})
	opt := Options{Repo: repo, Change: "main", Registry: registry, Out: out, Base: "main", Format: "shale", Clients: []llm.Client{client}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Questions, func(q Question) bool { return q.Kind == "conflict" && slices.Contains(q.Links, "world-old-1") })
	if i < 0 {
		t.Fatalf("questions = %+v; want a conflict resting on world-old-1", res.Questions)
	}
	q := res.Questions[i]
	src, _ := json.Marshal([]Answer{{Question: q.ID, Kind: q.Kind, Choice: "wrong_fact", Link: q.Link, Links: q.Links, Facts: q.Facts}})
	write(filepath.Join(out, answersFile), string(src))
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Retired, "world-old-1") {
		t.Fatalf("retired %v; want the approved fact", res.Retired)
	}
	for _, q := range res.Questions {
		if q.Kind == "conflict" {
			t.Fatalf("the conflict came back: %+v", q)
		}
	}
}

// What the owner says cannot happen reaches the world-fact reading, once,
// whether it comes from this change's answer or the lesson it left; the
// agent's own answers do not.
func TestOwnerFacts(t *testing.T) {
	as := []Answer{
		{Kind: "conflict", Choice: "impossible", Note: "no tool is lent to a stranger"},
		{Kind: "conflict", Choice: "impossible", Note: "agent guess", By: "agent"},
	}
	head := &ir.Registry{Lessons: []*ir.Lesson{{Kind: ir.LessonFact, Says: "This situation cannot happen in this project: no tool is lent to a stranger"}}}
	got := ownerFacts(as, head)
	if len(got) != 1 || got[0] != "The owner says: This situation cannot happen in this project: no tool is lent to a stranger" {
		t.Fatalf("got %q", got)
	}
}

// A state invariant tying a proposed attribute to state a step changes:
// the step writes the new attribute, so it can keep the invariant.
func TestStateInvariantWrites(t *testing.T) {
	dir := t.TempDir()
	src := "context: c\nformatVersion: 1\n---\nentity: Tool\ncontext: c\nattributes:\n  status: { type: enum, values: [available, lent] }\n  lentAt: { type: optional<time> }\n  dueAt: { type: optional<time> }\n---\noperation: Tool.lend\ncontext: c\ndefault: allow\nwrites: [Tool.status, Tool.lentAt]\n---\noperation: Tool.rename\ncontext: c\ndefault: allow\nwrites: []\n"
	if err := os.WriteFile(filepath.Join(dir, "v.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := vocab.Proposal{Extensions: []vocab.Entity{{Name: "Tool", Attributes: []vocab.Attribute{{Name: "dueAt"}}}}}
	cands := []translate.Candidate{{Outcome: "rule", Kind: ir.Invariant, Subject: "Tool", RuleID: "r1", When: "self.status = lent", Assert: "isSet(self.lentAt) and self.dueAt = self.lentAt + 14d"}}
	got := inferWrites(reg, p, cands)
	if strings.Join(got["Tool.lend"], ",") != "Tool.dueAt" || len(got["Tool.rename"]) != 0 {
		t.Fatalf("writes = %v; want Tool.lend to write Tool.dueAt", got)
	}
}

// A change's rule on an existing step that only reports values ("listing
// the tools shows each lent tool's due date") makes the step write
// nothing; one that sets a value still does (a developer trial,
// 2026-11-04: the listing wrote the id and the lent date).
func TestReportedValuesOfExistingStep(t *testing.T) {
	dir := t.TempDir()
	src := "context: c\nformatVersion: 1\n---\nentity: Tool\ncontext: c\nattributes:\n  id: { type: int }\n  status: { type: enum, values: [available, lent] }\n  lentAt: { type: optional<time> }\n---\nextend: Result\ncontext: c\nattributes:\n  dueOf: { type: \"map<int, time>\" }\n---\noperation: Tool.list\ncontext: c\ndefault: allow\nwrites: []\n---\noperation: Tool.lend\ncontext: c\ndefault: allow\nwrites: []\n"
	if err := os.WriteFile(filepath.Join(dir, "v.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cands := []translate.Candidate{
		{Outcome: "rule", Kind: ir.Invariant, Subject: "Tool", On: "Tool.list", RuleID: "r1", When: "self.status = lent and isSet(self.lentAt)", Assert: "has(result.dueOf, self.id) and result.dueOf[self.id] = self.lentAt + 14d"},
		{Outcome: "rule", Kind: ir.Invariant, Subject: "Tool", On: "Tool.lend", RuleID: "r2", Assert: "self.status = lent"},
	}
	got := inferWrites(reg, vocab.Proposal{}, cands)
	if len(got["Tool.list"]) != 0 || strings.Join(got["Tool.lend"], ",") != "Tool.status" {
		t.Fatalf("writes = %v; want only Tool.lend to write Tool.status", got)
	}
}

// When the check finds nothing wrong and only cannot decide a step, the
// owner is asked for the approval, told which step stays unchecked, instead
// of every later change touching that step being stuck.
func TestApprovalWhenOnlyUndecided(t *testing.T) {
	head := &ir.Registry{}
	res := &Result{Rules: map[string][]string{}, Statements: map[string]string{"intent/s1": "Lend tools"}, Sources: map[string]string{}, Added: []string{"intent/s1"},
		Gate: &gate.Result{Check: &checker.Result{Verdict: checker.Unknown, Undecided: []string{"Tool.lend"}}, Unapproved: []string{"intent/s1"}}}
	qs := questions(res, head, &cache{}, nil, nil)
	i := slices.IndexFunc(qs, func(q Question) bool { return q.Kind == "approve" })
	if i < 0 || !strings.Contains(qs[i].Text, "could not decide what Tool.lend must do") {
		t.Fatalf("questions = %+v; want the approval naming the undecided step", qs)
	}
}

// A request the owner retired for its restatement is that request, edited:
// no "the same requirement?" question, and its rules take a new version.
func TestRestatementIsTheSameRequirement(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := "Protect held resources: the system must not delete a resource with holds"
	write(filepath.Join(repo, ".shale/s1.yaml"), "id: s1\nintent:\n    title: '"+old+"'\n")
	write(filepath.Join(repo, ".shale/s2.yaml"), "id: s2\nintent:\n    title: 'Protect held resources: the system must not delete a resource with holds, unless an erasure request asks'\n")
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	write(filepath.Join(repo, ".stele/retired-intents.yaml"), "- id: intent/s1\n  by: owner\n  for: intent/s2\n")
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "rules.yaml"), "id: intent/s1\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: intent/s1\n  doc: .shale/s1.yaml\n  prose: '"+old+"'\n  textHash: "+translate.TextHash(old)+"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"intent/s1"}, "owner"); err != nil {
		t.Fatal(err)
	}
	client := one("Protect held", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds > 0 and request.kind != erasure"})
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "main", Registry: registry, Out: t.TempDir(), Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Renamed["intent/s1"] != "intent/s2" || len(res.Removed) != 0 || strings.Join(res.Modified, ",") != "intent/s2" {
		t.Fatalf("renamed %v removed %v modified %v", res.Renamed, res.Removed, res.Modified)
	}
	for _, q := range res.Questions {
		if q.Kind == "renamed" {
			t.Fatalf("asked %q", q.Text)
		}
	}
}

// A request that changes a number in an approved one, worded afresh, shares
// few words with it; its rule, the approved one renumbered, still asks
// whether it replaces it (a developer trial, 2026-11-03).
func TestRenumberedRuleRestates(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry := t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := "The system must not delete a resource with 3 or more holds"
	write(filepath.Join(repo, ".shale/s1.yaml"), "id: s1\nintent:\n    title: '"+old+"'\n")
	write(filepath.Join(repo, ".shale/s2.yaml"), "id: s2\nintent:\n    title: 'Storage is tight: two holds are now enough to keep a resource'\n")
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "rules.yaml"), "id: intent/s1\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds >= 3\nsource:\n  statementId: intent/s1\n  doc: .shale/s1.yaml\n  prose: '"+old+"'\n  textHash: "+translate.TextHash(old)+"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"intent/s1"}, "owner"); err != nil {
		t.Fatal(err)
	}
	client := one("Storage is tight", map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "deny", "when": "self.holds >= 2"})
	res, err := runKept(context.Background(), Options{Repo: repo, Change: "main", Registry: registry, Out: t.TempDir(), Base: "main", Format: "shale", Clients: []llm.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	var asked *Question
	for i, q := range res.Questions {
		if q.Kind == "restates" && len(q.Requirements) == 2 && q.Requirements[1].ID == "intent/s1" {
			asked = &res.Questions[i]
		}
	}
	if asked == nil || !strings.Contains(asked.Text, "changes a number") {
		t.Fatalf("no restates question for the renumbered rule: %+v", res.Questions)
	}
}

// A new request that adds an exception to an approved one can take
// precedence where they meet, both standing: the conflict offers it, the
// answer asks the translation for overrides, and a reading that names the
// earlier request there settles the conflict without retiring anything.
func TestPrecedenceKeepsBothRequests(t *testing.T) {
	for _, tool := range []string{"git", "z3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	repo, registry, out := t.TempDir(), t.TempDir(), t.TempDir()
	write := func(p, src string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	held := "Protect held resources: the system must not delete a resource with holds"
	write(filepath.Join(repo, ".shale/s1.yaml"), "id: s1\nintent:\n    title: '"+held+"'\n")
	write(filepath.Join(repo, ".shale/local/s2.jsonl"), `{"kind":"intent","at":"2026-10-02T10:00:00Z","title":"Honour erasure requests","body":"Erasure requests always delete the resource."}`+"\n")
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	write(filepath.Join(registry, "vocabulary.yaml"), vocabYAML)
	write(filepath.Join(registry, "rules.yaml"), "id: intent/s1\nkind: permission\neffect: deny\noperation: Resource.delete\nwhen: self.holds > 0\nsource:\n  statementId: intent/s1\n  doc: .shale/s1.yaml\n  prose: '"+held+"'\n  textHash: "+translate.TextHash(held)+"\n")
	reg, err := ir.LoadDir(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.ApproveRules(reg, []string{"intent/s1"}, "owner"); err != nil {
		t.Fatal(err)
	}
	reading := map[string]any{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"}
	opt := Options{Repo: repo, Change: "main", Registry: registry, Out: out, Base: "main", Format: "shale", Clients: []llm.Client{one("Erasure requests", reading)}}
	res, err := runKept(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Questions, func(q Question) bool { return q.Kind == "conflict" })
	if i < 0 || !slices.ContainsFunc(res.Questions[i].Options, func(o Option) bool { return o.Choice == "precedence" }) {
		t.Fatalf("questions = %+v; want a conflict offering precedence", res.Questions)
	}
	a := Answer{Question: res.Questions[i].ID, Kind: "conflict", Choice: "precedence", Over: []string{"intent/s1"}, Statements: map[string]string{"intent/s2": translate.TextHash(res.Statements["intent/s2"])}}
	if n := notes([]Answer{a}, translate.Statement{ID: "intent/s2", Prose: res.Statements["intent/s2"]}); len(n) != 1 || !strings.Contains(n[0], "list intent/s1 under overrides") {
		t.Fatalf("notes = %q", n)
	}
	src, _ := json.Marshal([]Answer{a})
	write(filepath.Join(out, answersFile), string(src))
	reading["overrides"] = []string{"intent/s1"} // the reading the note asks for
	opt.Clients = []llm.Client{one("Erasure requests", reading)}
	if res, err = runKept(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	for _, q := range res.Questions {
		if q.Kind == "conflict" {
			t.Fatalf("still a conflict: %s", q.Text)
		}
	}
	if len(res.Retired) != 0 || len(res.Removed) != 0 {
		t.Fatalf("retired %v removed %v; want both requests kept", res.Retired, res.Removed)
	}
}

// A threshold read through a new setting no requirement gives a value is
// asked about: checked for every value, it clashes with nothing (an overdue
// rule read with config.loanPeriod beside an approved due date of 14 days,
// a developer trial, 2026-11-03). A setting an invariant fixes is not.
func TestFreeSettingAsked(t *testing.T) {
	dir := t.TempDir()
	src := vocabYAML + `---
entity: Config
context: res
attributes:
  retention: { type: duration, setBy: world }
  grace: { type: duration, setBy: world }
---
id: intent/s1
kind: permission
effect: deny
operation: Resource.delete
when: config.retention > 0d
source: { statementId: intent/s1, doc: .shale/s1.yaml, prose: Keep resources for the retention period }
---
id: intent/s2
kind: invariant
assert: config.grace = 3d
source: { statementId: intent/s2, doc: .shale/s2.yaml, prose: The grace period is three days }
`
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	head, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := &Result{Format: "shale", Added: []string{"intent/s1"}, NewTerms: []string{"Config", "Config.retention", "Config.grace"},
		Rules: map[string][]string{"intent/s1": {"intent/s1"}}, Statements: map[string]string{"intent/s1": "Keep resources for the retention period"}}
	got := freeSettings(res, head)
	if len(got) != 1 || got[0].name != "Config.retention" {
		t.Fatalf("free settings %+v, want Config.retention only", got)
	}
	var asked bool
	for _, q := range questions(res, head, &cache{}, nil, nil) {
		asked = asked || q.Kind == "free_setting" && q.AskOwner && slices.Contains(q.Statements, "intent/s1")
	}
	if !asked {
		t.Error("no free_setting question")
	}
}

// A requirement checked in part says which part is not: a rule that never
// compiled (a loan history read through an entity the vocabulary lost) was
// dropped while the requirement's other rules made it look checked (a
// developer trial, 2026-11-03).
func TestPartlyCheckedRequirementSaysWhatIsNot(t *testing.T) {
	res := &Result{}
	c := translate.Candidate{StatementID: "intent/s1", Kind: ir.Invariant, On: "Tool.return", Assert: "any(r in instances(LoanRecord): r.toolId = self.id)",
		Error: "1: rule intent/s1.3: instances(LoanRecord): no such entity\nmore"}
	unusable(res, c)
	unusable(res, c)
	if fs := res.Findings["intent/s1"]; len(fs) != 1 || !strings.HasPrefix(fs[0], "not checked: ") || !strings.Contains(fs[0], "LoanRecord") || strings.Contains(fs[0], "more") {
		t.Errorf("findings %q", fs)
	}
}

// A coverage check that fails is told, not taken for a pass: the approval
// names the requirement as not judged, and the next check judges it again
// (a code review, 2026-11-03).
func TestFailedCoverageIsToldAndRetried(t *testing.T) {
	repo, registry := setup(t, "## ADDED Requirements\n\n### Requirement: Erasure\nErasure requests MUST always delete the resource.\n")
	out := t.TempDir()
	run := func(c fake) *Result {
		t.Helper()
		res, err := runKept(context.Background(), Options{Repo: repo, Change: "c1", Registry: registry, Out: out, Clients: []llm.Client{c}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	reading := map[string]reading{"Erasure": {rules: []map[string]any{{"kind": "permission", "operation": "Resource.delete", "effect": "allow", "when": "request.kind = erasure"}}}}
	res := run(fake{readings: reading, coverDown: true})
	if !slices.ContainsFunc(res.Findings["res/Erasure"], func(f string) bool { return strings.HasPrefix(f, "not judged: ") }) {
		t.Fatalf("findings %q; want the failed coverage named", res.Findings["res/Erasure"])
	}
	var calls []string
	res = run(fake{readings: reading, calls: &calls})
	if len(calls) == 0 {
		t.Error("the next check reused the unjudged reading instead of judging it again")
	}
	if slices.ContainsFunc(res.Findings["res/Erasure"], func(f string) bool { return strings.HasPrefix(f, "not judged: ") }) {
		t.Errorf("findings after a healthy check %q", res.Findings["res/Erasure"])
	}
}

func TestNewWorldFactAfterRejectedOne(t *testing.T) {
	links := []translate.Candidate{{RuleID: "world-main-1"}, {RuleID: "world-main-3"}, {RuleID: "world-def-0a1b2c3d"}}
	if n := lastNumber(links, "world-main-"); n != 3 {
		t.Errorf("lastNumber = %d, want 3", n)
	}
}

// runKept runs a check with every request declared with Shale kept as a
// requirement, for the tests about what Stele does with requirements.
func runKept(ctx context.Context, opt Options) (*Result, error) {
	if opt.Format == "shale" {
		if err := keepAll(opt.Repo); err != nil {
			return nil, err
		}
	}
	return Run(ctx, opt)
}

func keepAll(repo string) error {
	reqs, err := importer.ShaleRequests(repo)
	if err != nil {
		return nil // no requests: nothing to keep
	}
	var ks []importer.KeptIntent
	for _, r := range reqs {
		ks = append(ks, importer.KeptIntent{ID: r.ID})
	}
	return importer.KeepIntents(repo, ks, nil)
}
