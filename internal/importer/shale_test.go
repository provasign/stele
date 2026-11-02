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
	// A finalized session's log is older than its evidence file.
	write(".shale/local/s1.jsonl", `{"kind":"intent","at":"2026-10-01T09:00:00Z","title":"stale"}`+"\n")

	got, err := Shale(repo)
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
	got, err = Shale(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "intent/s3" {
		t.Fatalf("after retiring s1: %+v", got)
	}
}

func TestShaleWithoutEvidence(t *testing.T) {
	if _, err := Shale(t.TempDir()); !errors.Is(err, ErrNoSpecs) {
		t.Fatalf("err = %v, want ErrNoSpecs", err)
	}
}
