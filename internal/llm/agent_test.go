package llm

import (
	"os"
	"path/filepath"
	"testing"
)

// The backend agent is the coding agent Stele runs from and its model: the
// agent's environment names the agent, the hook's record the model.
func TestResolveAgentFollowsTheAgentInUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stele.yaml"), []byte("formatVersion: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(dir, ".stele", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	record := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(AgentRecordFile)), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("PATH", t.TempDir()) // no CLI installed

	// Run from Claude Code, the hook having recorded its model.
	t.Setenv("CLAUDECODE", "1")
	record(`{"agent":"claude","model":"claude-opus-5-5","session":"s1"}`)
	if b, m, ok := ResolveAgent(sub); !ok || b != "claude" || m != "claude-opus-5-5" {
		t.Errorf("from Claude Code with a record: %s %s %v", b, m, ok)
	}
	// Claude Code's environment outranks a record another agent left.
	record(`{"agent":"codex","model":"gpt-5.5","session":"s2"}`)
	if b, m, ok := ResolveAgent(sub); !ok || b != "claude" || m != "" {
		t.Errorf("from Claude Code with Codex's record: %s %s %v", b, m, ok)
	}
	// Run from Codex, known by its hook's record alone.
	t.Setenv("CLAUDECODE", "")
	if b, m, ok := ResolveAgent(sub); !ok || b != "codex" || m != "gpt-5.5" {
		t.Errorf("from Codex: %s %s %v", b, m, ok)
	}
	// Outside any agent, with no CLI and no key: nothing to run on.
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(AgentRecordFile))); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ResolveAgent(sub); ok {
		t.Error("resolved with no agent, CLI or key")
	}
	if _, _, err := Resolve("agent", ""); err == nil {
		t.Error("Resolve found a backend with no agent, CLI or key")
	}
	t.Setenv("OPENAI_API_KEY", "k")
	if b, _, ok := ResolveAgent(sub); !ok || b != "openai" {
		t.Errorf("with an API key alone: %s %v", b, ok)
	}
	// A model given outright is kept.
	t.Setenv("CLAUDECODE", "1")
	record(`{"agent":"claude","model":"claude-opus-5-5","session":"s1"}`)
	if b, m, err := Resolve("agent", "claude-sonnet-5-5"); err != nil || b != "claude" || m != "claude-sonnet-5-5" {
		t.Errorf("pinned model: %s %s %v", b, m, err)
	}
	if b, m, err := Resolve("codex", ""); err != nil || b != "codex" || m != "" {
		t.Errorf("a named backend changed: %s %s %v", b, m, err)
	}
}
