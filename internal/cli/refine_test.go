package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
)

func TestCarryOverDropsRetranslatedStatements(t *testing.T) {
	dir, next := t.TempDir(), t.TempDir()
	rules := "# header\nid: A\nkind: invariant\nsource:\n  statementId: s1\n---\nid: B\nkind: invariant\nsource:\n  statementId: s2\n---\nid: C\nkind: invariant\nsource:\n  statementId: s1\n"
	for name, src := range map[string]string{"rules.yaml": rules, "vocabulary.yaml": "entity: X\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vocab, err := carryOver(dir, next, 1, map[string][]string{"s1": {"note"}}, nil, map[string][]string{"s2": {"s9"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(vocab) != 1 || filepath.Base(vocab[0]) != "r1-vocabulary.yaml" {
		t.Errorf("vocabulary files = %v", vocab)
	}
	got, err := os.ReadFile(filepath.Join(next, "r1-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got); !strings.Contains(s, "overrides:\n    - s9") && !strings.Contains(s, "overrides:\n  - s9") {
		t.Errorf("overrides not added:\n%s", s)
	}
	if s := string(got); strings.Contains(s, "id: A") || strings.Contains(s, "id: C") || !strings.Contains(s, "id: B") || strings.HasPrefix(s, "\n---") {
		t.Errorf("carried rules:\n%s", s)
	}
}

func TestAddFactsWithNoneKept(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "v.yaml"), []byte("context: c\nformatVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept, err := addFacts(dir, 1, nil)
	if err != nil || len(kept) != 0 {
		t.Fatalf("no facts: kept %v, err %v", kept, err)
	}
	kept, err = addFacts(dir, 1, []worldFact{{Assert: "nosuch.attr = 1", Reason: "r"}})
	if err != nil || len(kept) != 0 {
		t.Fatalf("a fact that does not load: kept %v, err %v", kept, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "facts-1.yaml")); !os.IsNotExist(err) {
		t.Errorf("facts file left behind: %v", err)
	}
}

func TestStatementNamed(t *testing.T) {
	ids := []string{"009/FR-006", "009/FR-013", "010/FR-013"}
	for name, want := range map[string]string{"s2": "009/FR-013", "009/FR-006": "009/FR-006", "FR-006": "009/FR-006", "FR-013": "", "s9": "", "": ""} {
		if got := statementNamed(ids, name); got != want {
			t.Errorf("statementNamed(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWorldSet(t *testing.T) {
	dir := t.TempDir()
	vocab := "context: c\nformatVersion: 1\n---\nentity: Load\ncontext: c\nattributes:\n  outcome: { type: enum, values: [ok, httpError] }\n  exitCode: { type: int }\n---\noperation: Load.run\ncontext: c\ndefault: allow\nwrites: [Load.outcome, Load.exitCode]\n"
	if err := os.WriteFile(filepath.Join(dir, "vocabulary.yaml"), []byte(vocab), 0o644); err != nil {
		t.Fatal(err)
	}
	done, err := worldSet(dir, []string{"Load.outcome", "Load.missing"})
	if err != nil || len(done) != 1 || done[0] != "Load.outcome" {
		t.Fatalf("worldSet = %v, %v", done, err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := reg.Entities["Load"].Attrs["outcome"]
	if !a.WorldSet || reg.Operations["Load.run"].WritesAttr(a) || !reg.Operations["Load.run"].WritesAttr(reg.Entities["Load"].Attrs["exitCode"]) {
		t.Errorf("outcome world-set %v; run writes outcome %v", a.WorldSet, reg.Operations["Load.run"].WritesAttr(a))
	}
}
