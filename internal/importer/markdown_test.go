package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdown(t *testing.T) {
	dir := t.TempDir()
	src := "# Orders\n\nIntro text with no keyword.\n\n## Cancelling\n\n- An order MUST be refundable\n  until it ships.\n- **REQ-7**: Cancelled orders MUST NOT ship.\n- **REQ-8**: Orders have a number.\n\n```\ncode MUST be ignored\n```\n\n> Stele T-1: superseded\n\nA paragraph that\nSHALL span lines.\n\n| table | MUST skip |\n\n## 支払い\n\n- 支払いは MUST 記録される。\n- the system must log in lower case\n"
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "spec.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Markdown(dir, MarkdownOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ id, source, prose string }{
		{"docs/spec.md#orders-cancelling/1", "docs/spec.md#L7", "Section: Orders > Cancelling\nAn order MUST be refundable until it ships."},
		{"docs/spec.md#REQ-7", "docs/spec.md#L9", "Section: Orders > Cancelling\nREQ-7: Cancelled orders MUST NOT ship."},
		{"docs/spec.md#REQ-8", "docs/spec.md#L10", "Section: Orders > Cancelling\nREQ-8: Orders have a number."},
		{"docs/spec.md#orders-cancelling/4", "docs/spec.md#L18", "Section: Orders > Cancelling\nA paragraph that SHALL span lines."},
	}
	if len(got) != 5 {
		t.Fatalf("want 4 statements, got %d: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].ID != w.id || got[i].Source != w.source || got[i].Prose != w.prose {
			t.Errorf("statement %d = %+v, want %+v", i, got[i], w)
		}
	}
	if !strings.HasPrefix(got[4].ID, "docs/spec.md#orders-h") || !strings.HasSuffix(got[4].ID, "/1") {
		t.Errorf("non-ASCII section id: %s", got[4].ID)
	}
	any, _ := Markdown(dir, MarkdownOptions{AnyCase: true})
	if len(any) != 6 {
		t.Errorf("any case: want 6, got %d", len(any))
	}
}

// TestMarkdownMay: an uppercase MAY is a requirement, restricting ("MAY
// borrow only when") or permitting; a lowercase may in prose is not.
func TestMarkdownMay(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "lending.md"), []byte("# Lending\n\nYou may read this first.\n\n- A member MAY borrow a tool only when the tool is available.\n- Only a steward MAY mark a tool as under repair.\n- A member MUST NOT hold more than 3 tools.\n"), 0o644)
	stmts, err := Markdown(dir, MarkdownOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 3 {
		t.Fatalf("want the 3 requirements, got %d: %+v", len(stmts), stmts)
	}
}
