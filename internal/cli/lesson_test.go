package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLessonCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte("context: main\nformatVersion: 1\n---\nentity: Order\ncontext: main\nattributes:\n  paid: { type: bool }\n---\nid: S1\nkind: invariant\nassert: self.paid\nsubject: Order\nsource:\n  statementId: S1\n  textHash: h1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := cmdLesson([]string{"add", "--registry", dir, "--kind", "scope", "--says", "a record checked when it is created is enough here", "--by", "jane"}, &out); code != 0 {
		t.Fatalf("add: %d %s", code, out.String())
	}
	if code := cmdLesson([]string{"add", "--registry", dir, "--kind", "reading", "--says", "paid means captured, not authorised", "--statement", "S1", "--by", "jane"}, &out); code != 0 {
		t.Fatalf("add pinned: %d %s", code, out.String())
	}
	out.Reset()
	if code := cmdLesson([]string{"list", "--registry", dir}, &out); code != 0 || !strings.Contains(out.String(), "2 lesson(s)") || !strings.Contains(out.String(), "reading (S1)") || strings.Contains(out.String(), "stale") {
		t.Fatalf("list: %d\n%s", code, out.String())
	}
	// The statement's text changes: its lesson is listed as stale.
	src, _ := os.ReadFile(filepath.Join(dir, "r.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(strings.Replace(string(src), "textHash: h1", "textHash: h1\n  statementHash: h2", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	cmdLesson([]string{"list", "--registry", dir}, &out)
	if !strings.Contains(out.String(), "[stale") {
		t.Fatalf("a lesson pinned to old text should be stale:\n%s", out.String())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "lessons", "*.yaml"))
	id := strings.TrimSuffix(filepath.Base(files[0]), ".yaml")
	if code := cmdLesson([]string{"remove", id, "--registry", dir}, &out); code != 0 {
		t.Fatalf("remove: %d", code)
	}
	if files, _ = filepath.Glob(filepath.Join(dir, "lessons", "*.yaml")); len(files) != 1 {
		t.Fatalf("after remove: %v", files)
	}
}
