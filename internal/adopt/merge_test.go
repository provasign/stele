package adopt

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/lesson"
)

// Two branches that each approve rules and each learn a lesson merge with
// no git conflict, and the merged registry loads with both lessons and both
// approval records.
func TestParallelApprovalsAndLessonsMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := writeDir(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	git("init", "-q", "-b", "main")
	git("add", "-A")
	git("commit", "-q", "-m", "registry")
	work := func(branch, rule, says string) {
		t.Helper()
		git("checkout", "-q", "-b", branch, "main")
		reg, err := ir.LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := ApproveRules(reg, []string{rule}, branch); err != nil {
			t.Fatal(err)
		}
		if _, _, err := lesson.Add(dir, ir.Lesson{Kind: ir.LessonFact, Says: says, By: branch}); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", branch)
	}
	work("a", "S1", "a resource with holds is never deleted by the nightly sweep")
	work("b", "S2", "erasure requests come only from the privacy team")
	git("checkout", "-q", "main")
	git("merge", "-q", "--no-edit", "a")
	git("merge", "-q", "--no-edit", "b") // a conflict would fail here
	reg, err := ir.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Lessons) != 2 {
		t.Fatalf("merged lessons = %+v", reg.Lessons)
	}
	if !reg.Rule("S1").Approved() || !reg.Rule("S2").Approved() {
		t.Fatal("an approval was lost in the merge")
	}
	ledgers, _ := filepath.Glob(filepath.Join(dir, LedgerDir, "*.jsonl"))
	if len(ledgers) != 2 {
		t.Fatalf("ledger files = %v", ledgers)
	}
	if _, err := os.Stat(filepath.Join(dir, "approvals.jsonl")); err == nil {
		t.Fatal("the shared ledger file is still written")
	}
}
