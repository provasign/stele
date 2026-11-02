package config

import (
	"os"
	"path/filepath"
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
