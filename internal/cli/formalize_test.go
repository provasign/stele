package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/provasign/stele/internal/formalize"
	"github.com/provasign/stele/internal/ir"
	"github.com/provasign/stele/internal/llm"
)

func TestFormalizeResumePreservesContext(t *testing.T) {
	dir := t.TempDir()
	statements := filepath.Join(t.TempDir(), "statements.yaml")
	if err := os.WriteFile(statements, []byte("- id: S1\n  prose: Orders MUST be paid.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contextFile := filepath.Join(dir, "context.yaml")
	want := "# Owner's context\ncontext: main\nformatVersion: 1\n"
	if err := os.WriteFile(contextFile, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	// A legacy checkpoint is rejected before any model request. The context
	// must survive even when the CLI receives a different --context value.
	if err := os.WriteFile(filepath.Join(dir, "formalize-state.json"), []byte(`{"stage":"done"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--statements", statements, "--out", dir, "--one-pass", "--no-consolidate", "--llm", "cmd", "--llm-cmd", "exit 99"}
	var out bytes.Buffer
	if code := cmdFormalize(args, &out); code == 0 {
		t.Fatal("legacy state was accepted")
	}
	if code := cmdFormalize(append(args, "--context", "changed"), &out); code == 0 {
		t.Fatal("changed context reused the old run")
	}
	got, err := os.ReadFile(contextFile)
	if err != nil || string(got) != want {
		t.Fatalf("context was overwritten: %q, %v", got, err)
	}
}

func TestFormalizeCompactCLI(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(t.TempDir(), "statements.yaml")
	if err := os.WriteFile(input, []byte("- id: original-one\n  prose: Orders must be paid.\n- id: original-two\n  prose: Every order must be paid.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// This is a local fixture command, never a real model/agent process.
	reply := `{"entities":[{"name":"Order","statement_ids":["s1"],"attributes":[{"name":"paid","type":"bool","values":[],"min":0,"max":0,"set_by":"system"}],"relations":[]}],"extensions":[],"operations":[],"rules":[{"statement_id":"s1","kind":"invariant","subject":"","on":"","when":"","assert":"order.paid"}]}`
	command := "cat >/dev/null; printf '%s' '" + reply + "'"
	args := []string{"--statements", input, "--out", dir, "--compact", "--batch", "1", "--max-calls", "2", "--request-timeout", "5s", "--no-consolidate", "--cover=false", "--llm", "cmd", "--llm-cmd", command}
	var out bytes.Buffer
	if code := cmdFormalize(args, &out); code != 0 {
		t.Fatalf("compact CLI failed: %d, %s", code, out.String())
	}
	reg, err := ir.LoadDir(dir)
	if err != nil || reg.Rule("original-one") == nil || reg.Rule("original-two") == nil {
		t.Fatalf("lost original IDs: %+v, %v", reg, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, formalize.ReportFile))
	if err != nil {
		t.Fatal(err)
	}
	var res formalize.Result
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Calls.Started != 2 || res.Counts[formalize.Translated] != 2 {
		t.Fatalf("CLI ignored batch size: %+v", res)
	}
	// The budget is exhausted. A completed resume must still succeed.
	if code := cmdFormalize(args, &out); code != 0 {
		t.Fatalf("completed resume invoked model again: %d", code)
	}
}

// TestCodexTranslatesAtMediumEffort: translation asks Codex for medium
// effort unless --effort says otherwise, instead of the user's own Codex
// default; review keeps the CLI's default on Codex.
func TestCodexTranslatesAtMediumEffort(t *testing.T) {
	name := func(mk func(*flag.FlagSet) func() (llm.Client, error), args ...string) string {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		client := mk(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		c, err := client()
		if err != nil {
			t.Fatal(err)
		}
		return c.Name()
	}
	review := func(fs *flag.FlagSet) func() (llm.Client, error) {
		return llmFlagsEffort(fs, map[string]string{"claude": "high"})
	}
	for _, tc := range []struct{ got, want string }{
		{name(llmFlags, "--llm", "codex"), "codex-cli:default@medium"},
		{name(llmFlags, "--llm", "codex", "--effort", "high"), "codex-cli:default@high"},
		{name(llmFlags, "--llm", "claude"), "claude-cli:claude-sonnet-5-5"},
		{name(llmFlags, "--llm", "claude", "--model", "claude-opus-5-5"), "claude-cli:claude-opus-5-5"},
		{name(review, "--llm", "codex"), "codex-cli:default"},
		{name(review, "--llm", "claude"), "claude-cli:default@high"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %s, want %s", tc.got, tc.want)
		}
	}
}
