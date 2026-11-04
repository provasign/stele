package specsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/provasign/stele/internal/adopt"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/translate"
	"gopkg.in/yaml.v3"
)

const original = "# Door\n\nREQ-1: The door MUST be locked.\n"

func fixture(t *testing.T) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "specs")
	reg := filepath.Join(root, "registry")
	snapshot := filepath.Join(dir, ".stele", "statements.yaml")
	for _, d := range []string{root, reg, filepath.Dir(snapshot)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "door.md")
	write(t, file, original)
	stmts, err := importer.Markdown(root, importer.MarkdownOptions{Exclude: []string{reg}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(stmts)
	if err != nil {
		t.Fatal(err)
	}
	write(t, snapshot, string(data))
	vocab := filepath.Join(reg, "context.yaml")
	write(t, vocab, "context: c\nformatVersion: 1\n---\nentity: Door\ncontext: c\nattributes:\n  locked: {type: bool}\n")
	rep := &translate.Report{Candidates: []translate.Candidate{{StatementID: stmts[0].ID, RuleID: "R1", Outcome: "rule", Kind: "invariant", Assert: "door.locked"}}}
	if err := translate.WriteRegistry(reg, rep, stmts, []string{vocab}); err != nil {
		t.Fatal(err)
	}
	return Options{Root: root, Format: "markdown", Snapshot: snapshot, Registry: reg}, file
}

// write saves as editors do, through a temporary file renamed into place: a
// watcher polling in between would otherwise see the file empty, half
// written, and report that state.
func write(t *testing.T, path, text string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
func evaluate(t *testing.T, o Options) *Result {
	t.Helper()
	r, err := Evaluate(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSavedEditAndNewRequirementQueueOnlyChangedStatements(t *testing.T) {
	o, file := fixture(t)
	if r := evaluate(t, o); !r.InSync {
		t.Fatalf("initial registry not in sync: %+v", r)
	}
	before, _ := os.ReadFile(filepath.Join(o.Registry, "rules.yaml"))
	baseline, _ := os.ReadFile(o.Snapshot)
	write(t, file, strings.ReplaceAll(original, "locked", "unlocked")+"\nREQ-2: The door MUST support a key.\n")
	r := evaluate(t, o)
	if r.InSync || len(r.Changes.Added) != 1 || len(r.Changes.Changed) != 1 || len(r.Pending) != 2 || len(r.StaleRules) != 1 || r.StaleRules[0] != "R1" {
		t.Fatalf("edit/add not queued: %+v", r)
	}
	queue := filepath.Join(filepath.Dir(o.Snapshot), "pending.yaml")
	if err := WritePending(queue, r, o); err != nil {
		t.Fatal(err)
	}
	pending, err := translate.LoadStatements(queue)
	if err != nil || len(pending) != 2 {
		t.Fatalf("queue: %v, %v", pending, err)
	}
	after, _ := os.ReadFile(filepath.Join(o.Registry, "rules.yaml"))
	accepted, _ := os.ReadFile(o.Snapshot)
	if string(before) != string(after) || string(baseline) != string(accepted) {
		t.Fatal("detection mutated accepted rules or baseline")
	}
	if err := WritePending(o.Snapshot, r, o); err == nil {
		t.Fatal("queue overwrote baseline")
	}
	if err := WritePending(filepath.Join(o.Registry, "pending.yaml"), r, o); err == nil {
		t.Fatal("queue polluted live registry")
	}
	if err := WritePending(file, r, o); err == nil {
		t.Fatal("queue overwrote a spec file")
	}
}

func TestMoveDeletionAndFinalFileDeletionDoNotLoseIdentity(t *testing.T) {
	o, file := fixture(t)
	moved := filepath.Join(o.Root, "moved.md")
	if err := os.Rename(file, moved); err != nil {
		t.Fatal(err)
	}
	r := evaluate(t, o)
	if len(r.Changes.Moved) != 1 || len(r.Pending) != 0 || len(r.StaleRules) != 0 || len(r.Changes.Retire) != 0 {
		t.Fatalf("move was retranslated/retired: %+v", r)
	}
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	r = evaluate(t, o)
	if len(r.Changes.Removed) != 1 || len(r.Changes.Retire) != 1 || r.Changes.Retire[0] != "R1" || r.InSync {
		t.Fatalf("last-file removal not detected: %+v", r)
	}
	reg, err := ir.LoadDir(o.Registry)
	if err != nil || !reg.Rule("R1").Live() {
		t.Fatalf("watch retired rule automatically: %v", err)
	}
	for _, d := range r.Diagnostics {
		if d.Code == "spec.removed" && d.File == file {
			t.Fatal("deletion diagnostic points only at a nonexistent source")
		}
	}
}

func TestAdvancedSnapshotAndProvenanceDoNotHideStaleRule(t *testing.T) {
	o, file := fixture(t)
	write(t, file, strings.ReplaceAll(original, "locked", "unlocked"))
	cur, err := importer.Markdown(o.Root, importer.MarkdownOptions{Exclude: []string{o.Registry}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := yaml.Marshal(cur)
	write(t, o.Snapshot, string(data)) // prematurely accept the new source snapshot
	reg, err := ir.LoadDir(o.Registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := adopt.UpdateSources(reg, map[string]adopt.RuleSource{"R1": {StatementID: cur[0].ID, Doc: cur[0].Source, Hash: translate.TextHash(cur[0].Prose)}}); err != nil {
		t.Fatal(err)
	}
	r := evaluate(t, o)
	if r.InSync || len(r.Changes.Changed) != 0 || len(r.StaleRules) != 1 {
		t.Fatalf("stale translation hidden by provenance/snapshot: %+v", r)
	}
}

func TestAdvancedSnapshotDoesNotHideLiveRuleOfDeletedStatement(t *testing.T) {
	o, file := fixture(t)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	write(t, o.Snapshot, "[]\n") // deletion acknowledged before retiring its rule
	r := evaluate(t, o)
	if r.InSync || len(r.OrphanRules) != 1 || r.OrphanRules[0] != "R1" {
		t.Fatalf("orphaned rule hidden: %+v", r)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "rule.orphaned" {
		t.Fatalf("missing actionable rule diagnostic: %+v", r.Diagnostics)
	}
}

func TestGeneratedDocsAreExcludedAndLocationOnlyChangeDoesNotTranslate(t *testing.T) {
	o, file := fixture(t)
	write(t, filepath.Join(o.Registry, "coverage.md"), "The output MUST never become an input.\n")
	if r := evaluate(t, o); !r.InSync {
		t.Fatalf("generated docs imported: %+v", r)
	}
	write(t, file, "\n"+original)
	r := evaluate(t, o)
	if len(r.Pending) != 0 || len(r.StaleRules) != 0 || len(r.Changes.Sources) != 1 || r.InSync {
		t.Fatalf("line shift not treated as provenance-only: %+v", r)
	}
}

func TestWatchDetectsAtomicSaveAndClearsAfterRevert(t *testing.T) {
	o, file := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan *Result, 16)
	errs := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- Watch(ctx, o, 10*time.Millisecond, func(r *Result, err error) {
			if err != nil {
				select {
				case errs <- err:
				default:
				}
			} else {
				updates <- r
			}
		})
	}()
	next := func() *Result {
		t.Helper()
		select {
		case r := <-updates:
			return r
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("watch did not update")
		}
		return nil
	}
	if !next().InSync {
		t.Fatal("initial status not clean")
	}
	tmp := filepath.Join(o.Root, ".editor-save")
	write(t, tmp, strings.ReplaceAll(original, "locked", "unlocked"))
	if err := os.Rename(tmp, file); err != nil {
		t.Fatal(err)
	}
	if r := next(); len(r.Changes.Changed) != 1 {
		t.Fatalf("atomic save ignored: %+v", r)
	}
	write(t, file, original)
	if r := next(); !r.InSync || len(r.Diagnostics) != 0 {
		t.Fatalf("revert did not clear: %+v", r)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch failed to stop")
	}
}
