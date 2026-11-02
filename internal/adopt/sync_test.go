package adopt

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/translate"
)

func TestSyncPreservesRuleIdentityAcrossCycles(t *testing.T) {
	dir := writeDir(t)
	f := filepath.Join(dir, "r.yaml")
	text := strings.Replace(registry, "id: S2\n", "id: S2\nversion: 4\nsource:\n  doc: spec.md#L5\n  approvedBy: jane # preserve approval\n  textHash: original-hash\n", 1)
	if err := os.WriteFile(f, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	load := func() *ir.Registry {
		t.Helper()
		reg, err := ir.LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return reg
	}
	cur := append([]translate.Statement{}, stmts...)
	for i := range cur {
		cur[i].ID += "-moved"
		cur[i].Source = "spec.md#L" + []string{"5", "7", "9"}[i]
	}
	reg := load()
	rep := Sync(stmts, cur, reg)
	if len(rep.Moved) != 3 || len(rep.Sources) != 2 {
		t.Fatalf("first move: %+v", rep)
	}
	if err := UpdateSources(reg, rep.Sources); err != nil {
		t.Fatal(err)
	}
	reg = load()
	if x := reg.Rule("S2"); x == nil || x.Version != 4 || x.StatementID != "S2-moved" || x.Source != "spec.md#L7" || x.TextHash != "original-hash" {
		t.Fatalf("rule identity or translated evidence changed: %+v", x)
	}
	src, _ := os.ReadFile(f)
	if !strings.Contains(string(src), "approvedBy: jane # preserve approval") {
		t.Fatalf("source metadata or comment was lost:\n%s", src)
	}
	findings := score.CheckerFindings(&checker.Result{Conflicts: []checker.Conflict{{Rules: []string{"S1", "S2"}}}}, cur, reg)
	if !reflect.DeepEqual(findings[0].Sources, []string{"spec.md#L5", "spec.md#L7"}) {
		t.Errorf("findings used old IDs instead of current provenance: %+v", findings)
	}
	// The next import uses the moved statement IDs. An edit and a removal
	// must still find the original rule IDs and versions, even after reload.
	next := append([]translate.Statement{}, cur[1:]...)
	next[0].Prose += " Within 30 days."
	next[0].Source = "spec.md#L8"
	rep = Sync(cur, next, reg)
	if !reflect.DeepEqual(rep.Retire, []string{"S1"}) || !reflect.DeepEqual(rep.RulesOf["S2-moved"], []string{"S2@4"}) {
		t.Fatalf("second sync lost the original rules: %+v", rep)
	}
	if err := RetireRules(reg, rep.Retire); err != nil {
		t.Fatal(err)
	}
	if err := UpdateSources(reg, rep.Sources); err != nil {
		t.Fatal(err)
	}
	reg = load()
	if reg.Rule("S1").Live() || reg.Rule("S2").Version != 4 || reg.Rule("S2").Source != "spec.md#L8" {
		t.Fatalf("second apply changed rule identity: %+v", reg.Rules)
	}
	final := append([]translate.Statement{}, next...)
	final[0].ID = "S2-moved-again"
	final[0].Source = "spec.md#L10"
	rep = Sync(next, final, reg)
	if err := UpdateSources(reg, rep.Sources); err != nil {
		t.Fatal(err)
	}
	reg = load()
	rep = Sync(final, final[1:], reg)
	if !reflect.DeepEqual(rep.Retire, []string{"S2"}) {
		t.Fatalf("third move/removal lost the original rule: %+v", rep)
	}
}

func TestSyncRecognizesReusedLineAnchors(t *testing.T) {
	old := []translate.Statement{{ID: "spec:L3", Prose: "Original MUST remain."}, {ID: "spec:L4", Prose: "Second MUST remain."}}
	cur := []translate.Statement{{ID: "spec:L3", Prose: "New MUST be added."}, {ID: "spec:L4", Prose: old[0].Prose}, {ID: "spec:L5", Prose: old[1].Prose}}
	rep := Sync(old, cur, nil)
	if len(rep.Changed) != 0 || len(rep.Removed) != 0 || !reflect.DeepEqual(rep.Added, []string{"spec:L3"}) || rep.Moved["spec:L3"] != "spec:L4" || rep.Moved["spec:L4"] != "spec:L5" {
		t.Fatalf("line reuse was mistaken for edits: %+v", rep)
	}
}

func TestSyncDoesNotGuessDuplicateMoves(t *testing.T) {
	old := []translate.Statement{{ID: "S1", Prose: "Action MUST succeed."}, {ID: "S2", Prose: "Action MUST succeed."}}
	cur := []translate.Statement{{ID: "S3", Prose: "Action MUST succeed."}}
	rep := Sync(old, cur, nil)
	if len(rep.Ambiguous) != 2 || len(rep.Removed) != 0 || len(rep.Moved) != 0 || len(rep.Sources) != 0 {
		t.Fatalf("duplicate text must not pick an arbitrary identity or retire a rule: %+v", rep)
	}
}

func TestSyncKeepsDuplicateOccurrenceOrder(t *testing.T) {
	// A note shifts two identical requirements down one line. The first
	// one's new ID happens to reuse the second one's old ID.
	old := []translate.Statement{
		{ID: "S:L3", Source: "spec.md#L3", Prose: "Action MUST succeed."},
		{ID: "S:L4", Source: "spec.md#L4", Prose: "Action MUST succeed."},
	}
	cur := []translate.Statement{
		{ID: "S:L4", Source: "spec.md#L4", Prose: old[0].Prose},
		{ID: "S:L5", Source: "spec.md#L5", Prose: old[1].Prose},
	}
	rep := Sync(old, cur, nil)
	if rep.Unchanged != 0 || len(rep.Ambiguous) != 0 || len(rep.Moved) != 2 || rep.Moved["S:L3"] != "S:L4" || rep.Moved["S:L4"] != "S:L5" {
		t.Fatalf("duplicate occurrence identity was lost: %+v", rep)
	}
	// Multiplicity changes cannot identify the survivor from line IDs alone.
	rep = Sync(old, cur[:1], nil)
	if len(rep.Ambiguous) != 2 || len(rep.Retire) != 0 || len(rep.Moved) != 0 {
		t.Fatalf("changed duplicate count must remain ambiguous: %+v", rep)
	}
}

func TestSyncKeepsExplicitIdentityWhenTextIsCopied(t *testing.T) {
	old := []translate.Statement{{ID: "FR-001", Source: "spec.md#L3", Prose: "Action MUST succeed."}}
	cur := []translate.Statement{
		{ID: "FR-001", Source: "spec.md#L3", Prose: "Action MUST fail."},
		{ID: "FR-002", Source: "spec.md#L4", Prose: old[0].Prose},
	}
	rep := Sync(old, cur, nil)
	if !reflect.DeepEqual(rep.Changed, []string{"FR-001"}) || !reflect.DeepEqual(rep.Added, []string{"FR-002"}) || len(rep.Moved) != 0 {
		t.Fatalf("explicit ID lost to a copy of its old text: %+v", rep)
	}
	// With explicit IDs a duplicate's removal also has an unambiguous survivor.
	old = []translate.Statement{cur[1], {ID: "FR-003", Source: "spec.md#L5", Prose: old[0].Prose}}
	rep = Sync(old, cur[1:], nil)
	if len(rep.Ambiguous) != 0 || !reflect.DeepEqual(rep.Removed, []string{"FR-003"}) {
		t.Fatalf("explicit IDs should identify the removed duplicate: %+v", rep)
	}
}

func TestSyncApplyIsIdempotentWithReusedAnchors(t *testing.T) {
	dir := writeDir(t)
	text := strings.ReplaceAll(strings.ReplaceAll(registry, "id: S1", "id: spec:L3"), "id: S2", "id: spec:L4")
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	old := []translate.Statement{
		{ID: "spec:L3", Source: "spec.md#L3", Prose: "First MUST hold."},
		{ID: "spec:L4", Source: "spec.md#L4", Prose: "Second MUST hold."},
	}
	cur := []translate.Statement{
		{ID: "spec:L3", Source: "spec.md#L3", Prose: "New MUST hold."},
		{ID: "spec:L4", Source: "spec.md#L4", Prose: old[0].Prose},
		{ID: "spec:L5", Source: "spec.md#L5", Prose: old[1].Prose},
	}
	for attempt := 0; attempt < 2; attempt++ {
		reg, err := ir.LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		rep := Sync(old, cur, reg)
		if attempt == 1 && len(rep.Sources) != 0 {
			t.Fatalf("rerunning the same comparison shifted provenance again: %+v", rep)
		}
		if err := UpdateSources(reg, rep.Sources); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Rule("spec:L3").StatementID != "spec:L4" || reg.Rule("spec:L4").StatementID != "spec:L5" {
		t.Fatalf("stable rule identities drifted: %+v", reg.Rules)
	}
}
