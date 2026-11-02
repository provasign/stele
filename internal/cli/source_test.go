package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/provasign/stele/internal/config"
	"github.com/provasign/stele/internal/importer"
	"github.com/provasign/stele/internal/specsync"
	"github.com/provasign/stele/internal/translate"
	"gopkg.in/yaml.v3"
)

func TestWatchCLIUsesProjectConfigAndFailsDriftCheck(t *testing.T) {
	project := t.TempDir()
	specs := filepath.Join(project, "specs")
	reg := filepath.Join(project, "registry")
	for _, p := range []string{specs, reg} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(specs, "door.md")
	if err := os.WriteFile(file, []byte("REQ-1: The door MUST be locked.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stmts, err := importer.Markdown(specs, importer.MarkdownOptions{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(project, "statements.yaml")
	data, _ := yaml.Marshal(stmts)
	if err := os.WriteFile(snapshot, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reg, "context.yaml"), []byte("context: c\nformatVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Registry = "registry"
	cfg.Specs = &config.Specs{Root: "specs", Format: "markdown", Snapshot: "statements.yaml"}
	if err := config.Write(project, cfg); err != nil {
		t.Fatal(err)
	}
	args := []string{"--project", project, "--once", "--check", "--json"}
	var out bytes.Buffer
	if code := cmdWatch(args, &out); code != 0 {
		t.Fatalf("clean check failed: %d, %s", code, out.String())
	}
	if err := os.WriteFile(file, []byte("REQ-1: The door MUST be unlocked.\n\nREQ-2: Keys MUST work.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	queue := filepath.Join(project, "pending.yaml")
	if code := cmdWatch(append(args, "--out", queue), &out); code != 1 {
		t.Fatalf("drift did not fail coding-tool check: %d, %s", code, out.String())
	}
	var status specsync.Result
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.InSync || len(status.Pending) != 2 {
		t.Fatalf("machine-readable work missing: %+v", status)
	}
	pending, err := translate.LoadStatements(queue)
	if err != nil || len(pending) != 2 {
		t.Fatalf("translation queue: %v, %v", pending, err)
	}
}
