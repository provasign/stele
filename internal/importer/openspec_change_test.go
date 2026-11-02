package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/translate"
)

const truthSpec = `# Reminders

### Requirement: Due reminder
The system MUST send due reminders in a nightly batch.

### Requirement: Old name
The system MUST record every reminder sent.

### Requirement: Retired thing
The system MUST keep a state file.
`

const changeSpec = `## RENAMED Requirements
- FROM: ` + "`### Requirement: Old name`" + `
- TO: ` + "`### Requirement: Reminder log`" + `

## MODIFIED Requirements

### Requirement: Due reminder
The system MUST send each due reminder immediately, before the loan is overdue.

#### Scenario: Immediate send
- **WHEN** a loan falls due
- **THEN** the reminder is sent

## REMOVED Requirements

### Requirement: Retired thing
**Reason**: no longer needed.

## ADDED Requirements

### Requirement: Reminder opt-out
The system MUST let a member opt out of reminders.
`

func TestOpenSpecChange(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, src string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("openspec/specs/notifications/spec.md", truthSpec)
	write("openspec/changes/immediate-reminders/specs/notifications/spec.md", changeSpec)
	write("openspec/changes/archive/2026-01-01-old/specs/notifications/spec.md", changeSpec)

	ids, err := OpenSpecChanges(repo)
	if err != nil || strings.Join(ids, ",") != "immediate-reminders" {
		t.Fatalf("OpenSpecChanges = %v, %v", ids, err)
	}
	d, err := OpenSpecChange(repo, "immediate-reminders")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 1 || d.Added[0].ID != "notifications/Reminder opt-out" {
		t.Fatalf("added = %+v", d.Added)
	}
	if len(d.Modified) != 1 || !strings.Contains(d.Modified[0].Prose, "immediately") || !strings.Contains(d.Modified[0].Prose, "Scenario: Immediate send") {
		t.Fatalf("modified = %+v", d.Modified)
	}
	if strings.Join(d.Removed, ",") != "notifications/Retired thing" {
		t.Fatalf("removed = %v", d.Removed)
	}
	if d.Renamed["notifications/Old name"] != "notifications/Reminder log" {
		t.Fatalf("renamed = %v", d.Renamed)
	}
	if !strings.HasPrefix(d.Modified[0].Source, "openspec/changes/immediate-reminders/specs/notifications/spec.md#L") {
		t.Fatalf("source = %s", d.Modified[0].Source)
	}

	truth, err := OpenSpec(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	after, warn := d.Apply(truth)
	if len(warn) != 0 {
		t.Fatalf("warnings: %v", warn)
	}
	got := map[string]translate.Statement{}
	for _, s := range after {
		got[s.ID] = s
	}
	if _, ok := got["notifications/Retired thing"]; ok {
		t.Error("removed requirement kept")
	}
	if s := got["notifications/Reminder log"]; !strings.HasPrefix(s.Prose, "Requirement: Reminder log\n") || !strings.Contains(s.Prose, "record every reminder sent") {
		t.Errorf("renamed = %+v", s)
	}
	if s := got["notifications/Due reminder"]; !strings.Contains(s.Prose, "immediately") {
		t.Errorf("modified = %+v", s)
	}
	if _, ok := got["notifications/Reminder opt-out"]; !ok || len(after) != 3 {
		t.Errorf("after = %d statements", len(after))
	}
}

func TestOpenSpecChangeWarnings(t *testing.T) {
	d := &Delta{Modified: []translate.Statement{{ID: "x/Missing", Prose: "Requirement: Missing"}}, Removed: []string{"x/Gone"},
		Added: []translate.Statement{{ID: "x/Here", Prose: "Requirement: Here\nnew"}}}
	after, warn := d.Apply([]translate.Statement{{ID: "x/Here", Prose: "Requirement: Here\nold"}})
	if len(warn) != 3 || len(after) != 2 {
		t.Fatalf("warn = %v, after = %+v", warn, after)
	}
}
