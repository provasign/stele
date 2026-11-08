package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/provasign/stele/internal/change"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/ir"
)

// shaleProject is a repository with two requests declared with Shale: a
// rule the product keeps, and a chore.
func shaleProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".shale/config.yaml", "privacy: redacted\n")
	write(".shale/local/s1.jsonl", `{"kind":"intent","at":"2026-10-01T09:00:00Z","title":"Review the codebase"}
{"kind":"completion","at":"2026-10-01T09:30:00Z"}
{"kind":"intent","at":"2026-10-01T10:00:00Z","title":"Orders ship within 2 days","body":"An order placed before noon ships the same day."}
`)
	write("registry/context.yaml", "context: c\nformatVersion: 1\n---\nentity: Order\ncontext: c\nattributes:\n  shipped: {type: bool, controller: system}\n")
	cfg, _ := json.Marshal(map[string]any{"formatVersion": 1, "registry": "registry", "translate": map[string]any{"llm": "claude", "drafts": 1}, "specs": map[string]any{"root": ".", "format": "shale", "snapshot": "statements.yaml"}})
	write("stele.yaml", string(cfg))
	write("statements.yaml", "[]\n")
	return dir
}

// A request is a task until kept; the owner's no sets it aside for good,
// and the list shows each request's state, so an agent offers one once.
func TestKeepListAndDecline(t *testing.T) {
	dir := shaleProject(t)
	var out bytes.Buffer
	if code := cmdKeep([]string{"--project", dir, "--list"}, &out); code != 0 || !strings.Contains(out.String(), "task       intent/s1  Review the codebase") || !strings.Contains(out.String(), "task       intent/s1-c2  Orders ship within 2 days") {
		t.Fatalf("list: %d\n%s", code, out.String())
	}
	out.Reset()
	if code := cmdKeep([]string{"--project", dir, "Orders ship", "--by", "agent"}, &out); code != 0 || !strings.Contains(out.String(), `Kept "Orders ship within 2 days" (intent/s1-c2)`) {
		t.Fatalf("keep: %d\n%s", code, out.String())
	}
	out.Reset()
	if code := cmdKeep([]string{"--project", dir, "intent/s1", "--decline", "--by", "owner"}, &out); code != 0 || !strings.Contains(out.String(), "set aside as not a requirement") {
		t.Fatalf("decline: %d\n%s", code, out.String())
	}
	out.Reset()
	if code := cmdKeep([]string{"--project", dir, "--list"}, &out); code != 0 || !strings.Contains(out.String(), "set aside  intent/s1  Review") || !strings.Contains(out.String(), "kept       intent/s1-c2  Orders") {
		t.Fatalf("list after: %d\n%s", code, out.String())
	}
	specs, err := importer.Shale(dir)
	if err != nil || len(specs) != 1 || specs[0].ID != "intent/s1-c2" {
		t.Fatalf("specs = %v, %v; want the kept request alone", specs, err)
	}
	rs, _ := importer.RetiredIntents(dir)
	if len(rs) != 1 || rs[0].ID != "intent/s1" || rs[0].Why != "not a requirement" || rs[0].By != "owner" {
		t.Fatalf("retired = %+v", rs)
	}
}

// Export writes the kept requests as the first OpenSpec spec; the approved
// rules follow their statements under the new IDs, and stele.yaml names
// OpenSpec.
func TestExportOpenSpec(t *testing.T) {
	dir := shaleProject(t)
	if code := cmdKeep([]string{"--project", dir, "Orders ship", "--by", "agent"}, &bytes.Buffer{}); code != 0 {
		t.Fatal("keep")
	}
	prose := "Orders ship within 2 days\nAn order placed before noon ships the same day."
	sum := sha256.Sum256([]byte(prose))
	hash := hex.EncodeToString(sum[:])[:16]
	src, _ := json.Marshal(map[string]string{"statementId": "intent/s1-c2", "doc": ".shale/s1-c2.yaml", "textHash": hash, "statementHash": hash, "prose": prose})
	if err := os.WriteFile(filepath.Join(dir, "registry", "rules.yaml"), []byte("id: R1\nkind: invariant\nassert: order.shipped\nsource: "+string(src)+"\napproved: {by: owner, at: \"2026-10-02\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := cmdExport([]string{"openspec", "--project", dir}, &out); code != 0 || !strings.Contains(out.String(), "Orders ship within 2 days -> requests/Orders ship within 2 days") || !strings.Contains(out.String(), "1 rule(s) follow them") {
		t.Fatalf("export: %d\n%s", code, out.String())
	}
	spec, err := os.ReadFile(filepath.Join(dir, "openspec", "specs", "requests", "spec.md"))
	if err != nil || !strings.Contains(string(spec), "### Requirement: Orders ship within 2 days\nAn order placed before noon ships the same day.\n") {
		t.Fatalf("spec = %q, %v", spec, err)
	}
	stmts, err := importer.OpenSpec(dir, false)
	if err != nil || len(stmts) != 1 || stmts[0].ID != "requests/Orders ship within 2 days" {
		t.Fatalf("openspec import = %v, %v", stmts, err)
	}
	reg, err := ir.LoadDir(filepath.Join(dir, "registry"))
	if err != nil || len(reg.Rules) != 1 || reg.Rules[0].StatementID != "requests/Orders ship within 2 days" {
		t.Fatalf("registry after export: %v, %v", err, reg)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "stele.yaml"))
	if !strings.Contains(string(cfg), "format: openspec") {
		t.Fatalf("stele.yaml = %s", cfg)
	}
	if code := cmdExport([]string{"openspec", "--project", dir}, &out); code == 0 {
		t.Fatal("a second export must refuse: the spec exists and the format is no longer shale")
	}
}

// The owner summary says what Stele read, against what, and what is left.
func TestOwnerSummary(t *testing.T) {
	res := &change.Result{Added: []string{"intent/a", "intent/b"}, Modified: []string{"intent/c"}, Registry: t.TempDir(),
		Statements: map[string]string{"intent/a": "Orders ship within 2 days", "intent/b": "Refunds within 14 days", "intent/c": "Members borrow 5 books"},
		Unchecked:  map[string]string{"intent/b": "obligation"}, Findings: map[string][]string{"intent/a": {"no rule checks ..."}},
		Questions: []change.Question{{Kind: "conflict", AskOwner: true, Requirements: []change.Requirement{{Title: "Orders ship within 2 days"}, {Title: "Members borrow 5 books"}}}, {Kind: "self_conflict", AskOwner: false}}}
	got := ownerSummary(res, "questions")
	for _, want := range []string{"Stele read 3 requirements (2 new, 1 edited).", `Two requirements cannot both hold: "Orders ship within 2 days" and "Members borrow 5 books".`, `Stele cannot check "Refunds within 14 days" and would record it unchecked.`, "Its reading of 1 may say less or more than the text"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q\nmissing %q", got, want)
		}
	}
	if got := ownerSummary(&change.Result{Added: []string{"x"}, Statements: map[string]string{"x": "One rule"}, Registry: t.TempDir()}, "ready"); got != "Stele read 1 requirement. It agrees with the approved requirements." {
		t.Errorf("clean summary = %q", got)
	}
}
