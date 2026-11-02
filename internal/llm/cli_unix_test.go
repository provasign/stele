//go:build !windows

package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI puts an executable named name on PATH that records its arguments
// and runs reply.
func fakeCLI(t *testing.T, name, reply string) string {
	t.Helper()
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + args + "\ncat >/dev/null\n" + reply + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return args
}

func readArgs(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCLIEffort(t *testing.T) {
	req := Request{System: "s", Turns: []Turn{{Role: "user", Text: "u"}}, Schema: `{"type":"object"}`}

	args := fakeCLI(t, "claude", `echo '{"result":"","structured_output":{"ok":true}}'`)
	c, _ := New(Config{Backend: "claude", Model: "m", Effort: "low"})
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := readArgs(t, args); !strings.Contains(got, "--effort\nlow\n") {
		t.Errorf("claude args lack the effort:\n%s", got)
	}
	if c.Name() != "claude-cli:m@low" {
		t.Errorf("name %q", c.Name())
	}

	// The fake codex writes its final message to the -o path.
	args = fakeCLI(t, "codex", `while [ "$1" != "-o" ]; do shift; done; echo '{"ok":true}' > "$2"`)
	c, _ = New(Config{Backend: "codex", Effort: "minimal"})
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := readArgs(t, args); !strings.Contains(got, "-c\nmodel_reasoning_effort=minimal\n") {
		t.Errorf("codex args lack the effort:\n%s", got)
	}

	c, _ = New(Config{Backend: "codex"})
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := readArgs(t, args); strings.Contains(got, "model_reasoning_effort") || c.Name() != "codex-cli:default" {
		t.Errorf("default effort should pass nothing: %q\n%s", c.Name(), got)
	}
}
