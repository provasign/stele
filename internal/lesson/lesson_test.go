package lesson

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
)

func registryDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "context.yaml"), []byte("context: main\nformatVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAddLoadRemove(t *testing.T) {
	dir := registryDir(t)
	l := ir.Lesson{Kind: ir.LessonTerm, Says: "Customer and Account are the same person; use Account.", Terms: []string{"Customer", "Account"}, From: "stele change c1: same-x", By: "jane", At: "2026-10-31"}
	id, added, err := Add(dir, l)
	if err != nil || !added || !strings.HasPrefix(id, "L-") {
		t.Fatalf("add: %s %v %v", id, added, err)
	}
	if again, added, err := Add(dir, l); err != nil || added || again != id {
		t.Fatalf("adding the same lesson again: %s %v %v", again, added, err)
	}
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Lessons) != 1 || reg.Lessons[0].ID != id || reg.Lessons[0].Says != l.Says || len(reg.Lessons[0].Terms) != 2 {
		t.Fatalf("loaded %+v", reg.Lessons)
	}
	if err := Remove(dir, id); err != nil {
		t.Fatal(err)
	}
	if reg, err = ir.LoadDir(dir); err != nil || len(reg.Lessons) != 0 {
		t.Fatalf("after remove: %+v, %v", reg.Lessons, err)
	}
}

func TestLoaderRejectsBadLessons(t *testing.T) {
	for name, src := range map[string]string{
		"unpinned reading": "lesson: L-1\nkind: reading\nsays: read it as a ban\n",
		"unknown kind":     "lesson: L-2\nkind: hunch\nsays: x\n",
		"empty":            "lesson: L-3\nkind: fact\nsays: \"\"\n",
	} {
		dir := registryDir(t)
		if err := os.WriteFile(filepath.Join(dir, "l.yaml"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ir.LoadDir(dir); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestRelevantAndStale(t *testing.T) {
	pinned := &ir.Lesson{ID: "L-p", Kind: ir.LessonReading, Says: "read as a ban on the step", Statements: map[string]string{"S1": "h1"}}
	wide := &ir.Lesson{ID: "L-w", Kind: ir.LessonScope, Says: "a record checked when it is created is enough"}
	ls := []*ir.Lesson{pinned, wide}
	got := Relevant(ls, "S1", "h1", "text", 10)
	if len(got) != 2 {
		t.Fatalf("relevant to S1 as written: %v", got)
	}
	if got := Relevant(ls, "S1", "h2", "text", 10); len(got) != 1 || got[0] != wide {
		t.Fatalf("after S1 changed, its pinned lesson must not apply: %v", got)
	}
	if s := Stale(ls, map[string]string{"S1": "h2"}); len(s) != 1 || s[0] != pinned {
		t.Fatalf("stale = %v", s)
	}
	// Past MaxProjectWide, only lessons sharing words with the statement.
	var many []*ir.Lesson
	for i := 0; i <= MaxProjectWide; i++ {
		many = append(many, &ir.Lesson{ID: "x", Kind: ir.LessonFact, Says: "invoices are archived nightly"})
	}
	many = append(many, &ir.Lesson{ID: "L-dns", Kind: ir.LessonFact, Says: "mailbox aliases are created by the directory sync"})
	if got := Relevant(many, "S9", "h", "Each new employee MUST get mailbox aliases from the directory sync", 3); len(got) != 1 || got[0].ID != "L-dns" {
		t.Fatalf("relevant by words = %v", got)
	}
}

func TestContradictions(t *testing.T) {
	a := &ir.Lesson{ID: "L-a", Kind: ir.LessonTerm, Says: "Customer and Account are the same person", Terms: []string{"Customer", "Account"}}
	b := &ir.Lesson{ID: "L-b", Kind: ir.LessonTerm, Says: "an Account can hold several Customers; they are different", Terms: []string{"account"}}
	c := &ir.Lesson{ID: "L-c", Kind: ir.LessonFact, Says: "invoices are kept for seven years", Terms: []string{"Customer"}}
	got := Contradictions([]*ir.Lesson{a, b, c})
	if len(got) != 1 || got[0][0] != a || got[0][1] != b {
		t.Fatalf("contradictions = %v", got)
	}
}
