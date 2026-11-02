package adjudicate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/translate"
)

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "first spec"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-01-02T00:00:00Z", "GIT_AUTHOR_DATE=2026-01-02T00:00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestOutcomeFromDates(t *testing.T) {
	v := &Verdict{Kind: "superseded", Follows: []string{"new"}, Stale: []string{"old"},
		Dates: map[string]Dated{"new": {Date: "2026-05-01"}, "old": {Date: "2026-01-01"}}}
	if got := outcome(v); got != "newer_followed" {
		t.Errorf("newer followed: got %s", got)
	}
	v.Follows, v.Stale = []string{"old"}, []string{"new"}
	if got := outcome(v); got != "older_followed" {
		t.Errorf("older followed: got %s", got)
	}
	v.Dates["old"] = Dated{}
	if got := outcome(v); got != "followed_undated" {
		t.Errorf("undated: got %s", got)
	}
	if got := outcome(&Verdict{Kind: "compatible"}); got != "compatible" {
		t.Errorf("compatible: got %s", got)
	}
}

func TestGatherCiteAndDates(t *testing.T) {
	repo := gitRepo(t, map[string]string{
		"specs/a/spec.md":   "# A\n\n- FR-1: MUST retry once.\n- FR-2: MUST NOT retry.\n",
		"src/run.go":        "package run\n\n// retryLimit is the number of retries after a failure.\nconst retryLimit = 1\n",
		"src/run_test.go":   "package run\n\nfunc TestRetry() { _ = retryLimit }\n",
		"docs/notes.txt":    "retryLimit was 0 before\n",
		"node_modules/x.js": "retryLimit\n",
	})
	stmts := []translate.Statement{
		{ID: "a/FR-1", Source: "specs/a/spec.md#L3", Prose: "MUST retry once (`retry_limit`)."},
		{ID: "a/FR-2", Source: "specs/a/spec.md#L4", Prose: "MUST NOT retry."},
	}
	ev, err := Gather(repo, []string{"retryLimit", "MUST retry"}, EvidenceOptions{Exclude: SpecDirs([]string{stmts[0].Source})})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Hits["retryLimit"] != 4 || ev.Hits["MUST retry"] != 0 {
		t.Errorf("hits = %v, want 4 code hits and the spec text excluded", ev.Hits)
	}
	if len(ev.Excerpts) == 0 || !ev.Excerpts[0].Test {
		t.Errorf("tests should come first: %+v", ev.Excerpts)
	}
	if !checkCite(repo, Cite{File: "src/run.go", Line: 4, Quote: "const retryLimit = 1"}) {
		t.Error("exact quote on its line should check")
	}
	if checkCite(repo, Cite{File: "src/run.go", Line: 4, Quote: "const retryLimit = 2"}) {
		t.Error("an invented quote should not check")
	}
	if checkCite(repo, Cite{File: "../etc/passwd", Line: 1, Quote: "root"}) {
		t.Error("paths outside the repo are refused")
	}
	d := DateStatements(repo, stmts)
	if d["a/FR-1"].Date != "2026-01-02" || d["a/FR-1"].Subject != "first spec" {
		t.Errorf("dates = %+v", d)
	}
}

func TestHygiene(t *testing.T) {
	repo := gitRepo(t, map[string]string{
		"openspec/specs/cap/spec.md":                  "x\n",
		"openspec/specs/stray-change/specs/x/spec.md": "x\n",
		"src/main.rs": "let merge_queue = 1;\n",
	})
	stmts := []translate.Statement{
		{ID: "cap/Retry:L1", Source: "openspec/specs/cap/spec.md#L1", Prose: "Requirement: Retry\nMUST retry."},
		{ID: "cap/Retry:L9", Source: "openspec/specs/cap/spec.md#L9", Prose: "Requirement: Retry\nMUST NOT retry."},
		{ID: "cap/Queue", Source: "openspec/specs/cap/spec.md#L20", Prose: "SHALL use `merge_queue` and `stall_monitor`."},
	}
	h, err := CheckHygiene(repo, stmts)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Restated) != 1 || h.Restated[0].Title != "cap/Retry" {
		t.Errorf("restated = %+v", h.Restated)
	}
	if len(h.Misplaced) != 1 || h.Misplaced[0] != "openspec/specs/stray-change" {
		t.Errorf("misplaced = %+v", h.Misplaced)
	}
	if len(h.Unknown) != 1 || h.Unknown[0].Name != "stall_monitor" {
		t.Errorf("unknown = %+v", h.Unknown)
	}
}

func TestOverbroadIgnoresDates(t *testing.T) {
	v := &Verdict{Kind: "overbroad", Follows: []string{"old"}, Overbroad: []string{"new"},
		Dates: map[string]Dated{"new": {Date: "2026-05-01"}, "old": {Date: "2026-01-01"}}}
	if got := outcome(v); got != "overbroad" {
		t.Errorf("got %s, want overbroad", got)
	}
}

func TestGatherShowsDefinitions(t *testing.T) {
	body := "package lib\n\nfunc caller() { ResolveHost(\"x\") }\n\n" + strings.Repeat("// filler\n", 40) +
		"// ResolveHost tries GitLab before GitHub.\nfunc ResolveHost(uri string) string {\n\treturn \"gitlab\"\n}\n"
	repo := gitRepo(t, map[string]string{"lib/hosts.go": body})
	ev, err := Gather(repo, []string{"ResolveHost"}, EvidenceOptions{FilesPerTerm: 1, HitsPerFile: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ev.Render(), "return \"gitlab\"") {
		t.Errorf("the definition's body should be shown:\n%s", ev.Render())
	}
	ev, _ = Gather(repo, []string{"ResolveHost"}, EvidenceOptions{FilesPerTerm: 1, HitsPerFile: 1, NoDefinitions: true})
	if strings.Contains(ev.Render(), "return \"gitlab\"") {
		t.Error("NoDefinitions should show only the call site")
	}
}

func TestRenderRangesStaysInRepo(t *testing.T) {
	repo := gitRepo(t, map[string]string{"a.go": "one\ntwo\nthree\n"})
	got := RenderRanges(repo, []LineRange{{File: "a.go", From: 2, To: 3}, {File: "../x", From: 1, To: 2}, {File: "/etc/hosts", From: 1, To: 1}})
	if !strings.Contains(got, "     2  two") || strings.Contains(got, "../x") || strings.Contains(got, "/etc/hosts") {
		t.Errorf("got:\n%s", got)
	}
}

func TestMergeRanges(t *testing.T) {
	got := mergeRanges([]LineRange{{"a.go", -5, 10}, {"a.go", 11, 20}, {"b.go", 1, 5}, {"a.go", 40, 50}})
	want := []LineRange{{"a.go", 1, 20}, {"b.go", 1, 5}, {"a.go", 40, 50}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("mergeRanges = %v, want %v", got, want)
	}
}
