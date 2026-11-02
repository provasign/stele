package importer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const parallelSpec = `# job-runner Specification

## Purpose
Runs jobs. This MUST NOT become a statement.

## Requirements
### Requirement: Quota Rejection Handling

When the quota service rejects a job, the system MUST record the job as ` + "`blocked`" + `.

#### Scenario: rejection becomes blocked
- **WHEN** the scheduler reads the reply
- **THEN** the job is recorded as ` + "`blocked`" + `

### Requirement: Running Job Cancellation
キャンセルされなければならない（SHALL）。

### Requirement: Quota Rejection Handling
The first rejection can be retried.
`

const strayDelta = `## ADDED Requirements
### Requirement: Backlog Alert
The scheduler SHALL provide backlog_alert configuration.
`

func TestOpenSpec(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"openspec/specs/job-runner/spec.md":                              parallelSpec,
		"openspec/specs/scheduler-alerts/specs/configuration/spec.md":    strayDelta,
		"openspec/changes/archive/2026-01-01-x/specs/job-runner/spec.md": strayDelta, // not merged truth
	}
	for name, src := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stmts, err := OpenSpec(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ id, source, prose string }{
		{"job-runner/Quota Rejection Handling:L7", "openspec/specs/job-runner/spec.md#L7",
			"Requirement: Quota Rejection Handling\nWhen the quota service rejects a job, the system MUST record the job as `blocked`.\n\n#### Scenario: rejection becomes blocked\n- WHEN the scheduler reads the reply\n- THEN the job is recorded as `blocked`"},
		{"job-runner/Running Job Cancellation", "openspec/specs/job-runner/spec.md#L15",
			"Requirement: Running Job Cancellation\nキャンセルされなければならない（SHALL）。"},
		{"job-runner/Quota Rejection Handling:L18", "openspec/specs/job-runner/spec.md#L18",
			"Requirement: Quota Rejection Handling\nThe first rejection can be retried."},
		{"scheduler-alerts/specs/configuration/Backlog Alert", "openspec/specs/scheduler-alerts/specs/configuration/spec.md#L2",
			"Requirement: Backlog Alert\nThe scheduler SHALL provide backlog_alert configuration."},
	}
	if len(stmts) != len(want) {
		t.Fatalf("got %d statements, want %d: %+v", len(stmts), len(want), stmts)
	}
	for i, w := range want {
		if s := stmts[i]; s.ID != w.id || s.Source != w.source || s.Prose != w.prose {
			t.Errorf("statement %d =\n%+v\nwant\n%+v", i, s, w)
		}
	}
}

func TestOpenSpecDates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	p := filepath.Join(repo, "openspec/specs/job-runner/spec.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(date string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("2026-01-18T12:00:00Z", "init", "-q")
	write := func(src string) {
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("## Requirements\n### Requirement: A\nOld text.\n### Requirement: B\nB text.\n")
	git("2026-01-18T12:00:00Z", "add", ".")
	git("2026-01-18T12:00:00Z", "commit", "-q", "-m", "one")
	write("## Requirements\n### Requirement: A\nNew text.\n### Requirement: B\nB text.\n")
	git("2026-01-18T15:00:00Z", "commit", "-q", "-am", "two") // the same day, a later commit

	stmts, err := OpenSpec(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range stmts {
		got[s.ID] = s.Introduced
	}
	if got["job-runner/A"] != "2026-01-18T15:00:00Z" || got["job-runner/B"] != "2026-01-18T12:00:00Z" {
		t.Errorf("introduced = %v, want A 2026-01-18T15:00:00Z (edited), B 2026-01-18T12:00:00Z", got)
	}
}
