package codegraph

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefineAndExclude(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	src := "package pager\n\n// Launch starts the pager.\nfunc Launch(cmd string) error {\n\tif cmd == \"\" {\n\t\treturn nil\n\t}\n\treturn run(cmd)\n}\n\nfunc run(string) error { return nil }\n"
	if err := os.WriteFile(filepath.Join(repo, "pager.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	g, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	defs := g.Define("Launch")
	if len(defs) != 1 || defs[0].File != "pager.go" || defs[0].From > 4 || defs[0].To < 9 {
		t.Fatalf("Launch defined at %+v", defs)
	}
	if len(g.Define("Lau")) != 0 {
		t.Fatal("a part of a name matched")
	}
	st, _ := exec.Command("git", "-C", repo, "status", "--porcelain").Output()
	if strings.Contains(string(st), ".grove") {
		t.Fatalf("the index shows in git status: %s", st)
	}
}
