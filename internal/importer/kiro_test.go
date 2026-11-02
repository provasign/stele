package importer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKiro(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"build-only/requirements.md":  "# Requirements Document\n\n## Introduction\n\nThe script SHALL be described here.\n\n## Glossary\n\n- **Build_Script**: The shell script that builds.\n- **Publish_Phase**: The publishing steps.\n\n## Requirements\n\n### Requirement 1: Build-only mode\n\n**User Story:** As a developer, I want a build, so that I SHALL see it.\n\n#### Acceptance Criteria\n\n1. WHEN `SKIP_PUBLISH` is `1`, THE Build_Script SHALL skip the Publish_Phase.\n2. WHILE build-only, THE Build_Script SHALL\n   skip tagging.\n3. THE Build_Script SHALL accept:\n   - `--json`\n   - `--quiet`\n",
		"build-only/design.md":        "# Design\n\nThe cache SHALL be keyed by hash.\n",
		"build-only/tasks.md":         "- [ ] 1. Implement it; the code SHALL compile.\n",
		"lock-leak/bugfix.md":         "# Bugfix Requirements Document\n\n## Bug Analysis\n\n### Current Behavior (Defect)\n\n1.1 WHEN the open raises, THEN the lock SHALL stay held.\n\n### Expected Behavior (Correct)\n\n2.1 WHEN the open raises, THEN the lock SHALL be released.\n\n### Unchanged Behavior (Regression Prevention)\n\n3.1 A successful open SHALL return the same file.\n",
		"archive/old/requirements.md": "### Requirement 1: Old\n\n1. THE system SHALL do the old thing.\n",
	}
	for name, src := range files {
		p := filepath.Join(repo, ".kiro", "specs", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Kiro(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ id, source, prose string }{
		{"build-only/R1.1", ".kiro/specs/build-only/requirements.md#L20", "Requirement 1: Build-only mode\nWHEN `SKIP_PUBLISH` is `1`, THE Build_Script SHALL skip the Publish_Phase.\nGlossary: Build_Script: The shell script that builds. | Publish_Phase: The publishing steps."},
		{"build-only/R1.2", ".kiro/specs/build-only/requirements.md#L21", "Requirement 1: Build-only mode\nWHILE build-only, THE Build_Script SHALL skip tagging.\nGlossary: Build_Script: The shell script that builds."},
		{"build-only/R1.3", ".kiro/specs/build-only/requirements.md#L23", "Requirement 1: Build-only mode\nTHE Build_Script SHALL accept: `--json`; `--quiet`\nGlossary: Build_Script: The shell script that builds."},
		{"lock-leak/2.1", ".kiro/specs/lock-leak/bugfix.md#L11", "2.1 WHEN the open raises, THEN the lock SHALL be released."},
		{"lock-leak/3.1", ".kiro/specs/lock-leak/bugfix.md#L15", "3.1 A successful open SHALL return the same file."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d statements, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if s := got[i]; s.ID != w.id || s.Source != w.source || s.Prose != w.prose {
			t.Errorf("statement %d = %+v, want %+v", i, s, w)
		}
	}
}
