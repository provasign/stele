package importer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestShaleReadsFinalizedAndOpenIntents(t *testing.T) {
	repo := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		p = filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".shale/config.yaml", "privacy: redacted\n")
	write(".shale/s1.yaml", `shale_version: "0"
id: s1
intent:
    title: Limit loans to 5 books
    body: A member may never have more than 5 books on loan.
    declared_at: 2026-10-01T10:00:00Z
`)
	write(".shale/s2.yaml", "shale_version: \"0\"\nid: s2\nnotes: []\n") // no intent declared
	write(".shale/s3.yaml", `id: s3
intent:
    title: Reminders go out a day before the due date
    declared_at: 2026-10-02T10:00:00Z
`)
	// An open session that declared its request twice: the latest counts.
	write(".shale/local/s4.jsonl", `{"kind":"intent","at":"2026-10-03T10:00:00Z","title":"Summer reading: 8 books"}
{"kind":"file_touch","at":"2026-10-03T10:01:00Z","path":"a.go"}
not json
{"kind":"intent","at":"2026-10-03T10:02:00Z","title":"Summer reading program","body":"During it, members can have up to 10 books on loan."}
`)

	got, err := ShaleRequests(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ id, prose, source, introduced string }{
		{"intent/s1", "Limit loans to 5 books\nA member may never have more than 5 books on loan.", ".shale/s1.yaml", "2026-10-01"},
		{"intent/s3", "Reminders go out a day before the due date", ".shale/s3.yaml", "2026-10-02"},
		{"intent/s4", "Summer reading program\nDuring it, members can have up to 10 books on loan.", ".shale/s4.yaml", "2026-10-03"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d statements, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.id || g.Prose != w.prose || g.Source != w.source || g.Introduced != w.introduced {
			t.Errorf("statement %d = %+v, want %+v", i, g, w)
		}
	}

	// The owner retires the 5-book limit: it is no longer read.
	if err := RetireIntents(repo, []RetiredIntent{{ID: "intent/s1", By: "owner", For: "intent/s4"}}); err != nil {
		t.Fatal(err)
	}
	if err := RetireIntents(repo, []RetiredIntent{{ID: "intent/s1", By: "someone else"}}); err != nil {
		t.Fatal(err)
	}
	rs, err := RetiredIntents(repo)
	if err != nil || len(rs) != 1 || rs[0].By != "owner" {
		t.Fatalf("retired = %+v, %v; want the first retirement only", rs, err)
	}
	got, err = ShaleRequests(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "intent/s3" {
		t.Fatalf("after retiring s1: %+v", got)
	}
}

// TestShaleContinuation: a request declared in a session whose evidence was
// already finalized is a request of its own, named as Shale names its
// continuation file.
func TestShaleContinuation(t *testing.T) {
	repo := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		p = filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".shale/s1.yaml", "id: s1\nfinalized_at: 2026-10-01T10:05:00Z\nintent:\n    title: Orders ship within 2 days\n    declared_at: 2026-10-01T10:00:00Z\n")
	write(".shale/local/s1.jsonl", `{"kind":"prompt","at":"2026-10-01T11:00:00Z","text":"..."}
{"kind":"intent","at":"2026-10-01T11:00:05Z","title":"Orders ship within 3 days"}
`)
	got, err := ShaleRequests(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "intent/s1" || got[1].ID != "intent/s1-c2" || got[1].Source != ".shale/s1-c2.yaml" || got[1].Prose != "Orders ship within 3 days" {
		t.Fatalf("got %+v; want s1, then s1-c2 with the new request", got)
	}

	// Once finalized, the continuation reads the same; a third request
	// takes the next name.
	write(".shale/s1-c2.yaml", "id: s1-c2\nfinalized_at: 2026-10-01T11:10:00Z\nintent:\n    title: Orders ship within 3 days\n    declared_at: 2026-10-01T11:00:05Z\n")
	write(".shale/local/s1.jsonl", `{"kind":"intent","at":"2026-10-01T12:00:00Z","title":"Orders ship the same day"}`+"\n")
	got, err = ShaleRequests(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[1].ID != "intent/s1-c2" || got[2].ID != "intent/s1-c3" {
		t.Fatalf("got %+v; want s1, s1-c2, s1-c3", got)
	}
}

func TestShaleWithoutEvidence(t *testing.T) {
	if _, err := ShaleRequests(t.TempDir()); !errors.Is(err, ErrNoSpecs) {
		t.Fatalf("err = %v, want ErrNoSpecs", err)
	}
}

// An agent works through two features in one conversation: a completion
// closes the first request, and the next is a request of its own, named as
// Shale names its file. A request declared again before the completion
// restates it.
func TestShaleTasksInOneSession(t *testing.T) {
	repo := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		p = filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".shale/config.yaml", "privacy: redacted\n")
	write(".shale/local/s1.jsonl", `{"kind":"intent","at":"2026-11-03T03:23:17Z","title":"Create tasks"}
{"kind":"intent","at":"2026-11-03T03:24:00Z","title":"Create and persist tasks"}
{"kind":"completion","at":"2026-11-03T03:53:47Z","note":"feature 1 done"}
{"kind":"prompt","at":"2026-11-03T03:54:00Z","text":"next: list"}
{"kind":"intent","at":"2026-11-03T03:54:28Z","title":"List saved tasks"}
`)
	got, err := ShaleRequests(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "intent/s1" || got[0].Prose != "Create and persist tasks" || got[1].ID != "intent/s1-c2" || got[1].Source != ".shale/s1-c2.yaml" || got[1].Prose != "List saved tasks" {
		t.Fatalf("got %+v; want the restated first request as s1 and the next as s1-c2", got)
	}

	// Shale finalizes the session into s1.yaml and s1-c2.yaml; a third task
	// in the new log is the next name.
	write(".shale/s1.yaml", "id: s1\nfinalized_at: 2026-11-03T04:00:00Z\nintent:\n    title: Create and persist tasks\n    declared_at: 2026-11-03T03:24:00Z\n")
	write(".shale/s1-c2.yaml", "id: s1-c2\nfinalized_at: 2026-11-03T04:00:00Z\nintent:\n    title: List saved tasks\n    declared_at: 2026-11-03T03:54:28Z\n")
	write(".shale/local/s1.jsonl", `{"kind":"intent","at":"2026-11-03T04:10:00Z","title":"Complete a task"}`+"\n")
	if got, err = ShaleRequests(repo); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "intent/s1" || got[1].ID != "intent/s1-c2" || got[2].ID != "intent/s1-c3" {
		t.Fatalf("after finalizing: %+v", got)
	}
}

// A commit recorded the closed task while the next one was open: the open
// request, older than that record, is still read; and the agent's session
// ending for good closes a task as shale done does.
func TestShaleOpenTaskAfterCommitTimeRecord(t *testing.T) {
	repo := t.TempDir()
	write := func(p, s string) {
		t.Helper()
		p = filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".shale/s1.yaml", "id: s1\nfinalized_at: 2026-11-03T10:30:00Z\nintent:\n    title: Create tasks\n    declared_at: 2026-11-03T10:00:00Z\n")
	write(".shale/local/s1.jsonl", `{"kind":"session_meta","at":"2026-11-03T10:00:00Z","tool":"codex"}
{"kind":"intent","at":"2026-11-03T10:10:00Z","title":"List tasks"}
{"kind":"session_close","at":"2026-11-03T10:40:00Z"}
`)
	got, err := ShaleRequests(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "intent/s1" || got[1].ID != "intent/s1-c2" || got[1].Prose != "List tasks" {
		t.Fatalf("got %+v", got)
	}
}

// A conversation exited and resumed closes and reopens the same session, as
// each turn of claude -p --resume does: the request declared again after it
// restates the task, as Shale folds it, and is not a second request.
func TestShaleResumedSessionRestates(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".shale", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := `{"kind":"intent","at":"2026-11-03T03:23:17Z","title":"Limit loans to 3"}
{"kind":"session_end","at":"2026-11-03T03:24:00Z"}
{"kind":"session_close","at":"2026-11-03T03:24:00Z"}
{"kind":"session_meta","at":"2026-11-03T03:30:00Z"}
{"kind":"intent","at":"2026-11-03T03:31:00Z","title":"Limit loans to 3 at a time"}
{"kind":"session_close","at":"2026-11-03T03:32:00Z"}
`
	if err := os.WriteFile(filepath.Join(repo, ".shale", "local", "s1.jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ShaleRequests(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "intent/s1" || got[0].Prose != "Limit loans to 3 at a time" {
		t.Fatalf("got %+v; want the restated request alone", got)
	}
}

// A request is a task until it is kept: only kept requests are specs, and
// the requests a project approved before are kept the first time.
func TestShaleReadsOnlyKeptRequests(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".shale", "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := `{"kind":"intent","at":"2026-10-01T09:00:00Z","title":"Review the codebase"}
{"kind":"completion","at":"2026-10-01T09:30:00Z"}
{"kind":"intent","at":"2026-10-01T10:00:00Z","title":"Orders ship within 2 days"}
`
	if err := os.WriteFile(filepath.Join(repo, ".shale", "local", "s1.jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := Shale(repo); err != nil || len(got) != 0 {
		t.Fatalf("nothing kept: got %v, %v; want no specs", got, err)
	}
	if err := KeepApproved(repo, []string{"intent/s1-c2", "res/Other"}); err != nil {
		t.Fatal(err)
	}
	got, err := Shale(repo)
	if err != nil || len(got) != 1 || got[0].ID != "intent/s1-c2" {
		t.Fatalf("after carrying the approved over: got %v, %v; want intent/s1-c2", got, err)
	}
	// The file exists now: a later approval list changes nothing by itself.
	if err := KeepApproved(repo, []string{"intent/s1"}); err != nil {
		t.Fatal(err)
	}
	if err := KeepIntents(repo, []KeptIntent{{ID: "intent/s1"}}, []string{"intent/s1-c2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Shale(repo); len(got) != 1 || got[0].ID != "intent/s1" {
		t.Fatalf("after keeping s1 and dropping s1-c2: got %v", got)
	}
}
