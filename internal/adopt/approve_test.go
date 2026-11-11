package adopt

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/gate"
	"github.com/provasign/stele/internal/ir"
)

func TestApproveRulesPinsText(t *testing.T) {
	dir := writeDir(t)
	f := filepath.Join(dir, "r.yaml")
	src := strings.Replace(registry, "id: S2\n", "id: S2\nsource:\n  doc: spec.md#L5\n  textHash: h1 # translated text\n", 1)
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
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
	reg := load()
	if got := gate.Unapproved(nil, reg); strings.Join(got, ",") != "S1,S2" {
		t.Fatalf("unapproved before = %v", got)
	}
	if err := ApproveRules(reg, []string{"S1", "S2"}, "jane"); err != nil {
		t.Fatal(err)
	}
	reg = load()
	if !reg.Rule("S2").Approved() || !reg.Rule("S1").Approved() {
		t.Fatalf("not approved: %+v %+v", reg.Rule("S1"), reg.Rule("S2"))
	}
	if got := gate.Unapproved(nil, reg); len(got) != 0 {
		t.Fatalf("unapproved after = %v", got)
	}
	out, _ := os.ReadFile(f)
	if !strings.Contains(string(out), "# translated text") || !strings.Contains(string(out), "approvedHash: h1") {
		t.Fatalf("approval not pinned or comment lost:\n%s", out)
	}
	// A re-translation from changed text no longer carries the approval.
	if err := os.WriteFile(f, []byte(strings.Replace(string(out), "textHash: h1", "textHash: h2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	base := reg
	reg = load()
	if reg.Rule("S2").Approved() {
		t.Fatal("approval survived a text change")
	}
	if got := gate.Unapproved(base, reg); strings.Join(got, ",") != "S2" {
		t.Fatalf("unapproved after text change = %v", got)
	}
	if err := RemoveRules(reg, []string{"S1"}); err != nil {
		t.Fatal(err)
	}
	if load().Rule("S1") != nil {
		t.Fatal("S1 not removed")
	}
}

// Reading the same text into a different rule needs approving again, and
// every approval is in the ledger with the reading it approved.
func TestApprovalPinsReadingAndIsRecorded(t *testing.T) {
	dir := writeDir(t)
	f := filepath.Join(dir, "r.yaml")
	src := strings.Replace(registry, "id: S1\n", "id: S1\nsource:\n  doc: spec.md#L3\n  textHash: h1\n", 1)
	if err := os.WriteFile(f, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApproveRules(reg, []string{"S1"}, "jane"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(f)
	if !strings.Contains(string(out), "approvedReading: "+reg.Rule("S1").ReadingHash()) {
		t.Fatalf("reading not pinned:\n%s", out)
	}
	ledgerText := func() string {
		files, _ := filepath.Glob(filepath.Join(dir, LedgerDir, "*.jsonl"))
		var all []byte
		for _, f := range files {
			b, _ := os.ReadFile(f)
			all = append(all, b...)
		}
		return string(all)
	}
	ledger := []byte(ledgerText())
	if strings.Count(string(ledger), "\n") != 1 || !strings.Contains(string(ledger), `"by":"jane"`) || !strings.Contains(string(ledger), `"rule":"S1"`) || !strings.Contains(string(ledger), "(self.holds > 0)") {
		t.Fatalf("ledger = %s, %v", ledger, err)
	}
	// Same text, new reading: the approval no longer holds.
	if err := os.WriteFile(f, []byte(strings.Replace(string(out), "when: self.holds > 0", "when: self.holds > 1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	base := reg
	reg, err = ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Rule("S1").Approved() {
		t.Fatal("approval survived a new reading of the same text")
	}
	if got := gate.Unapproved(base, reg); strings.Join(got, ",") != "S1" {
		t.Fatalf("unapproved = %v", got)
	}
	// A second approval appends; the first stays.
	if err := ApproveRules(reg, []string{"S1"}, "jane"); err != nil {
		t.Fatal(err)
	}
	if ledger = []byte(ledgerText()); strings.Count(string(ledger), "\n") != 2 {
		t.Fatalf("ledger after a second approval:\n%s", ledger)
	}
}

// Moving an invariant to another step changes what it means, so the gate
// counts it as a changed rule.
func TestReadingIncludesStep(t *testing.T) {
	a := &ir.Rule{Kind: ir.Invariant, On: "Order.cancel"}
	b := &ir.Rule{Kind: ir.Invariant, On: "Order.ship"}
	if a.ReadingHash() == b.ReadingHash() {
		t.Fatal("rules on different steps read the same")
	}
}

// TestApprovalNamesVocabularyChanges: approving under a changed vocabulary
// records the change on each rule it affects, and keeps those an earlier
// approval recorded.
func TestApprovalNamesVocabularyChanges(t *testing.T) {
	base, err := ir.LoadDir(writeDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ApproveRules(base, []string{"S1"}, "jane"); err != nil {
		t.Fatal(err)
	}
	if base, err = ir.LoadDir(filepath.Dir(strings.SplitN(base.Rule("S1").File, ":", 2)[0])); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src, _ := os.ReadFile(strings.SplitN(base.Rule("S1").File, ":", 2)[0])
	os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(strings.Replace(string(src), "range: [0, 5]", "range: [0, 9]", 1)), 0o644)
	load := func() *ir.Registry {
		t.Helper()
		reg, err := ir.LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return reg
	}
	head := load()
	if got := gate.Unapproved(base, head); !slices.Contains(got, "S1") {
		t.Fatalf("a wider range under an approved rule: unapproved = %v", got)
	}
	if err := ApproveRulesUnder(head, []string{"S1"}, "jane", gate.MeaningChanges(base, head)); err != nil {
		t.Fatal(err)
	}
	head = load()
	if got := gate.Unapproved(base, head); slices.Contains(got, "S1") || len(head.Rule("S1").ApprovedChanges) != 1 {
		t.Fatalf("after approval: unapproved = %v, changes = %v", got, head.Rule("S1").ApprovedChanges)
	}
	// Approved again for another reason, the rule keeps the change it carries.
	if err := ApproveRules(head, []string{"S1"}, "jane"); err != nil {
		t.Fatal(err)
	}
	if got := gate.Unapproved(base, load()); slices.Contains(got, "S1") {
		t.Fatalf("a later approval dropped the change: %v", got)
	}
}
