package bench

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/provasign/stele/internal/translate"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSetsResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sets.yaml"), "- name: a\n  statements: s.yaml\n  known: k.yaml\n  heldOut: true\n")
	sets, err := LoadSets(filepath.Join(dir, "sets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sets[0].Statements; got != filepath.Join(dir, "s.yaml") {
		t.Errorf("statements resolved to %s", got)
	}
	if sets[0].Reference != "" || !sets[0].HeldOut {
		t.Errorf("got %+v", sets[0])
	}
	write(t, filepath.Join(dir, "dup.yaml"), "- name: a\n  statements: s\n- name: a\n  statements: t\n")
	if _, err := LoadSets(filepath.Join(dir, "dup.yaml")); err == nil {
		t.Error("duplicate set names accepted")
	}
}

// Two overlapping calls, a pause between sessions, then a third call that
// fails: in-flight time excludes the pause, and peak concurrency is 2.
func TestJournalTiming(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "requests.jsonl"), `{"id":1,"event":"started","at":"2026-10-25T10:00:00Z","backend":"codex-cli:x","prompt_bytes":100}
{"id":2,"event":"started","at":"2026-10-25T10:00:05Z","backend":"codex-cli:x","prompt_bytes":50}
{"id":1,"event":"completed","at":"2026-10-25T10:00:20Z","reply_bytes":10,"elapsed_ms":20000,"usage":{"input_tokens":7,"output_tokens":3}}
{"id":2,"event":"completed","at":"2026-10-25T10:00:30Z","reply_bytes":20,"elapsed_ms":25000}
{"id":3,"event":"started","at":"2026-10-25T11:00:00Z","backend":"codex-cli:x"}
{"id":3,"event":"failed","at":"2026-10-25T11:00:10Z","elapsed_ms":10000}
{"id":4,"event":"started","at":"2026-10-25T11:00:10Z","backend":"codex-cli:x"}
`)
	tm, err := journal(filepath.Join(dir, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if tm.ActiveSeconds != 40 || tm.CallSeconds != 55 || tm.LongestCall != 25 || tm.MaxInFlight != 2 {
		t.Errorf("timing %+v", tm)
	}
	if c := tm.Calls; c.Started != 4 || c.Completed != 2 || c.Failed != 1 || c.Interrupted != 1 {
		t.Errorf("calls %+v", c)
	}
	if tm.PromptBytes != 150 || tm.ReplyBytes != 30 || tm.Usage.InputTokens != 7 || len(tm.Backends) != 1 {
		t.Errorf("totals %+v", tm)
	}
	if tm, err := journal(filepath.Join(dir, "none.jsonl")); tm != nil || err != nil {
		t.Errorf("missing journal: %v %v", tm, err)
	}
}

// Statements outside the set are ignored; statements the run never reached
// count as absent, and only translated and informal ones as checked.
func TestCoverage(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "formalize.json"), `{"statements":[
{"id":"a","state":"translated"},
{"id":"b","state":"needs_vocabulary","missing":["ordering","strings"]},
{"id":"c","state":"informal"},
{"id":"d","state":"partial","missing":["ordering"]},
{"id":"other","state":"translated"}]}`)
	stmts := []translate.Statement{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}, {ID: "e"}}
	rep := &Report{}
	if err := rep.coverage(dir, stmts); err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 2 || rep.States["absent"] != 1 || rep.States["translated"] != 1 {
		t.Errorf("states %v checked %d", rep.States, rep.Checked)
	}
	if len(rep.Missing) != 2 || rep.Missing[0] != (Count{"ordering", 2}) {
		t.Errorf("missing %v", rep.Missing)
	}
}
