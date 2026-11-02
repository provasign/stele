package importer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpecKit(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, "specs", "014-shared-hosts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := "# Spec\n\n- **FR-009c**: System MUST NOT auto-stop **idle** nodes.\n- **FR-010**: Users can rename nodes.\n\nThe API MUST return 404 for unknown nodes.\n\n- **FR-010**: duplicate id\n- **FR-011**: Every node MUST\n  report its region.\n\nA worktree name:\n\n- MUST NOT begin with `-`.\n- MUST NOT contain `..`.\n\n- **FR-012**: The CLI MUST accept:\n  - `--json`\n  - `--quiet`\n\n| ID | Trigger | Response |\n|----|---------|----------|\n| FR-013 | WHEN asked | THE SYSTEM SHALL answer |\n"
	model := "| field | note |\n- Max lifetime applies regardless of status\n- [ ] Nodes SHALL record their region\n"
	for name, src := range map[string]string{"spec.md": spec, "data-model.md": model} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stmts, err := SpecKit(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ id, source, prose string }{
		{"014/data-model.md:L3", "specs/014-shared-hosts/data-model.md#L3", "Nodes SHALL record their region"},
		{"014/FR-009c", "specs/014-shared-hosts/spec.md#L3", "FR-009c: System MUST NOT auto-stop idle nodes."},
		{"014/FR-010", "specs/014-shared-hosts/spec.md#L4", "FR-010: Users can rename nodes."},
		{"014/spec.md:L6", "specs/014-shared-hosts/spec.md#L6", "The API MUST return 404 for unknown nodes."},
		{"014/FR-010:L8", "specs/014-shared-hosts/spec.md#L8", "FR-010: duplicate id"},
		{"014/FR-011", "specs/014-shared-hosts/spec.md#L9", "FR-011: Every node MUST report its region."},
		{"014/spec.md:L14", "specs/014-shared-hosts/spec.md#L14", "A worktree name: MUST NOT begin with `-`."},
		{"014/spec.md:L15", "specs/014-shared-hosts/spec.md#L15", "A worktree name: MUST NOT contain `..`."},
		{"014/FR-012", "specs/014-shared-hosts/spec.md#L17", "FR-012: The CLI MUST accept: `--json`; `--quiet`"},
		{"014/FR-013", "specs/014-shared-hosts/spec.md#L23", "FR-013: Trigger: WHEN asked; Response: THE SYSTEM SHALL answer"},
	}
	if len(stmts) != len(want) {
		t.Fatalf("got %d statements, want %d: %+v", len(stmts), len(want), stmts)
	}
	for i, w := range want {
		if s := stmts[i]; s.ID != w.id || s.Source != w.source || s.Prose != w.prose {
			t.Errorf("statement %d = %+v, want %+v", i, s, w)
		}
	}
}
