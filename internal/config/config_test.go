package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A stele.yaml written by hand may leave out translate.drafts: it means
// the default, as stele init writes it.
func TestMissingDraftsIsTheDefault(t *testing.T) {
	dir := t.TempDir()
	src := "formatVersion: 1\nregistry: .stele/registry\ntranslate:\n  llm: claude\n  model: claude-sonnet-5-5\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Find(dir)
	if err != nil || c.Translate.Drafts != Default().Translate.Drafts {
		t.Fatalf("config = %+v, %v", c, err)
	}
}

// TestPathsStayInTheProject: stele.yaml cannot point the registry, the
// specs or the snapshot outside the project.
func TestPathsStayInTheProject(t *testing.T) {
	for _, c := range []Config{
		{FormatVersion: 1, Registry: "../elsewhere", Translate: Translate{LLM: "agent", Drafts: 1}},
		{FormatVersion: 1, Registry: "r", Translate: Translate{LLM: "agent", Drafts: 1}, Specs: &Specs{Root: "/etc", Snapshot: "s.yaml", Format: "markdown"}},
		{FormatVersion: 1, Registry: "r", Translate: Translate{LLM: "agent", Drafts: 1}, Specs: &Specs{Root: "specs", Snapshot: "../../s.yaml", Format: "markdown"}},
	} {
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "outside the project") {
			t.Errorf("%+v: %v", c, err)
		}
	}
	ok := Config{FormatVersion: 1, Registry: ".stele/registry", Translate: Translate{LLM: "agent", Drafts: 1}, Specs: &Specs{Root: ".", Snapshot: ".stele/s.yaml", Format: "markdown"}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a project's own paths: %v", err)
	}
}
