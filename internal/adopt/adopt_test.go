package adopt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/checker"
	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/score"
	"github.com/provasign/stele/internal/solver"
	"github.com/provasign/stele/internal/translate"
)

// Two statements a year apart conflict: the older denies deleting a resource
// with holds, the newer allows erasure. A third repeats the newer one.
const registry = `context: c
formatVersion: 1
---
extend: platform.Request
context: c
attributes:
  kind: { type: enum, values: [normal, erasure] }
---
entity: Resource
context: c
attributes:
  holds: { type: int, range: [0, 5] }
---
operation: Resource.delete
context: c
default: allow
writes: []
---
id: S1
kind: permission
effect: deny
operation: Resource.delete
when: self.holds > 0
---
id: S2
kind: permission
effect: allow
operation: Resource.delete
when: request.kind = erasure
`

var stmts = []translate.Statement{
	{ID: "S1", Source: "spec.md#L3", Prose: "Resources with holds MUST NOT be deleted.", Introduced: "2025-01-10"},
	{ID: "S2", Source: "spec.md#L5", Prose: "Users MUST be able to erase their data.", Introduced: "2025-07-02"},
	{ID: "S3", Source: "spec.md#L7", Prose: "Erasure MUST always succeed.", Introduced: "2025-07-02"},
}

const spec = "# Spec\n\n- Resources with holds MUST NOT be deleted.\n\n- Users MUST be able to erase their data.\n\n- Erasure MUST always succeed.\n"

func writeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func needZ3(t *testing.T) {
	if _, err := exec.LookPath("z3"); err != nil {
		t.Skip("z3 not on PATH")
	}
}

func check(t *testing.T, dir string) (*ir.Registry, *checker.Result) {
	t.Helper()
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := run(context.Background(), &solver.Z3{}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return reg, res
}

func TestDraftDirectionAndGroups(t *testing.T) {
	reg, err := ir.LoadDir(writeDir(t))
	if err != nil {
		t.Fatal(err)
	}
	res := &checker.Result{Conflicts: []checker.Conflict{{Unit: "Resource.delete", Rules: []string{"S1", "S2"}}}}
	cands := []score.Candidate{
		{Statements: []string{"S1", "S2"}, Verdict: score.Confirmed, Situation: "erasure of a held resource"},
		{Statements: []string{"S2", "S3"}, Verdict: score.NotReproduced, Situation: "same day"},
		{Statements: []string{"S1", "S4"}, Verdict: score.Unproved, Situation: "no date"},
	}
	items := Draft(cands, res, reg, stmts)
	if len(items) != 3 {
		t.Fatalf("want 3 items (the checker conflict is the first candidate's), got %+v", items)
	}
	first := items[0]
	if first.Newer != "S2" || first.SupersededBy["S1"] != "S2" || strings.Join(first.Retire, ",") != "S1@1" || strings.Join(first.RetireIfReversed, ",") != "S2@1" {
		t.Errorf("the newer statement should supersede: %+v", first)
	}
	if !strings.HasPrefix(first.Group, "G-1 (shares S") || items[1].Group != first.Group {
		t.Errorf("two items sharing a statement form the first group: %q %q", first.Group, items[1].Group)
	}
	for _, it := range items[1:] {
		if it.Newer != "" {
			t.Errorf("%v: no direction expected (same day or no date): %+v", it.Statements, it)
		}
	}
}

// Project B's X2: a finding cites three statements, and the checker finds the
// oldest in conflict with each of the other two. The middle one wins its own
// conflict and must not be retired because the newest is in the finding too.
func TestDraftDirectsEachConflict(t *testing.T) {
	dir := writeDir(t)
	s6 := "---\nid: S6\nkind: permission\neffect: allow\noperation: Resource.delete\nwhen: request.kind = normal\n"
	f, err := os.OpenFile(filepath.Join(dir, "r.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(s6)
	f.Close()
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]translate.Statement{}, stmts...), translate.Statement{ID: "S6", Source: "spec.md#L9", Introduced: "2025-09-01"})
	res := &checker.Result{Conflicts: []checker.Conflict{{Rules: []string{"S1", "S2"}}, {Rules: []string{"S1", "S6"}}}}
	items := Draft([]score.Candidate{{Statements: []string{"S1", "S2", "S6"}, Verdict: score.Confirmed}}, res, reg, all)
	it := items[0]
	if it.Newer != "S6" || strings.Join(it.Retire, ",") != "S1@1" || it.SupersededBy["S1"] != "S6" || it.SupersededBy["S2"] != "" {
		t.Errorf("only S1 loses; S2 beats it and S6 never conflicts with S2: %+v", it)
	}
}

func TestBaselineThenRatchet(t *testing.T) {
	needZ3(t)
	dir := writeDir(t)
	reg, res := check(t, dir)
	cands := []score.Candidate{{Statements: []string{"S1", "S2"}, Verdict: score.Confirmed, Situation: "erasure of a held resource"}}
	items := Draft(cands, res, reg, stmts)
	if _, err := Baseline(context.Background(), &solver.Z3{}, dir, items, stmts, Options{By: []string{"jane"}, Since: "2026-10-25"}); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending items must be refused: %v", err)
	}

	// Left open: the conflict becomes an open conflict and the gate passes.
	items[0].Decision = Open
	got, err := Baseline(context.Background(), &solver.Z3{}, dir, items, stmts, Options{By: []string{"jane"}, Since: "2026-10-25"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Open != 1 || got.Decisions != 0 || got.Verdict != checker.Pass {
		t.Errorf("want one open conflict and a pass, got %+v", got)
	}
	g, err := gate.Dirs(context.Background(), &solver.Z3{}, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Accepted || len(g.Open) != 1 || g.Open[0].Status != gate.OpenTolerated || !strings.Contains(strings.Join(g.Open[0].Rules, ","), "S1@1") {
		t.Errorf("the baseline should pass the gate with OC-1 tolerated: %+v", g)
	}

	// Accepted: the older rule is retired and nothing is left open.
	items[0].Decision = Accept
	got, err = Baseline(context.Background(), &solver.Z3{}, dir, items, stmts, Options{By: []string{"jane"}, Since: "2026-10-25"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Open != 0 || got.Decisions != 1 || got.Retired != 1 || got.Verdict != checker.Pass {
		t.Errorf("want one decision retiring S1 and nothing open, got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ConflictsFile)); !os.IsNotExist(err) {
		t.Errorf("a rerun must replace the earlier open conflicts: %v", err)
	}
}

func TestWriteBack(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "spec.md"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []Item{{ID: "T-1", Statements: []string{"S1", "S2"}, Newer: "S2", Older: []string{"S1"}, SupersededBy: map[string]string{"S1": "S2"},
		Retire: []string{"S1@1"}, Decision: Accept, Situation: "erasure of a held resource"}}
	for range 2 { // the second run finds the note and adds nothing
		if _, err := WriteBack(repo, items, stmts, false); err != nil {
			t.Fatal(err)
		}
	}
	src, _ := os.ReadFile(filepath.Join(repo, "spec.md"))
	lines := strings.Split(string(src), "\n")
	want := "  > Stele T-1: superseded by S2 where they conflict (decision D-T-1): erasure of a held resource"
	if lines[3] != want || strings.Count(string(src), "> Stele") != 1 {
		t.Errorf("note not written once under S1:\n%s", src)
	}
}

func TestSync(t *testing.T) {
	needZ3(t)
	dir := writeDir(t)
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cur := []translate.Statement{
		{ID: "S2", Source: "spec.md#L3", Prose: "Users MUST be able to erase their data, within 30 days."}, // changed
		{ID: "S3b", Source: "spec.md#L5", Prose: "Erasure MUST always succeed."},                           // moved
		{ID: "S5", Source: "spec.md#L7", Prose: "Exports MUST be signed."},                                 // added
	} // S1 removed
	rep := Sync(stmts, cur, reg)
	if strings.Join(rep.Removed, ",") != "S1" || strings.Join(rep.Changed, ",") != "S2" || rep.Moved["S3"] != "S3b" || strings.Join(rep.Added, ",") != "S5" {
		t.Fatalf("report = %+v", rep)
	}
	if strings.Join(rep.Retire, ",") != "S1" || strings.Join(rep.RulesOf["S2"], ",") != "S2@1" {
		t.Errorf("rules: retire %v, changed %v", rep.Retire, rep.RulesOf)
	}
	if err := RetireRules(reg, rep.Retire); err != nil {
		t.Fatal(err)
	}
	reg, res := check(t, dir)
	if reg.Rule("S1").Status != "retired" || len(res.Conflicts) != 0 {
		t.Errorf("retiring S1 should remove the conflict: %s, %+v", reg.Rule("S1").Status, res.Conflicts)
	}
}
