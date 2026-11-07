package llm

import (
	"os"
	"path/filepath"
	"testing"
)

// The backend agent is the coding agent Stele runs from and its model: the
// agent's marks on the commands it runs name the agent, the hook's record
// of that session the model.
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
	t.Chdir(sub) // Resolve reads the record of the project it runs in
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("PATH", t.TempDir()) // no CLI installed
	check := func(what, wantB, wantM string) {
		t.Helper()
		if b, m, ok := ResolveAgent(sub); !ok || b != wantB || m != wantM {
			t.Errorf("%s: %s %q %v, want %s %q", what, b, m, ok, wantB, wantM)
		}
	}

	// Run from Claude Code, the hook having recorded its session's model.
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	record(`{"agent":"claude","model":"claude-opus-5-5","session":"s1"}`)
	check("Claude Code with its record", "claude", "claude-opus-5-5")
	// A record another session left names no model for this one.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s9")
	check("Claude Code with an old session's record", "claude", "")
	// Claude Code's marks outrank a record Codex left.
	record(`{"agent":"codex","model":"gpt-5.5","session":"c1"}`)
	check("Claude Code with Codex's record", "claude", "")

	// Run from Codex: its marks, with or without the hook's record.
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_THREAD_ID", "c1")
	check("Codex with its record", "codex", "gpt-5.5")
	record(`{"agent":"claude","model":"claude-opus-5-5","session":"s1"}`)
	check("Codex whose hooks never ran", "codex", "")
	// Codex run inside a Claude Code terminal: the record of this session decides.
	t.Setenv("CLAUDECODE", "1")
	record(`{"agent":"codex","model":"gpt-5.5","session":"c1"}`)
	check("Codex inside Claude Code", "codex", "gpt-5.5")

	// Outside any agent, the record names the agent last used.
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CODEX_THREAD_ID", "")
	check("outside an agent, with a record", "codex", "gpt-5.5")
	// With no record, no CLI and no key: nothing to run on.
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
	check("an API key alone", "openai", "")
	// A model given outright is kept; a named backend is left alone.
	t.Setenv("CLAUDECODE", "1")
	if b, m, err := Resolve("agent", "claude-sonnet-5-5"); err != nil || b != "claude" || m != "claude-sonnet-5-5" {
		t.Errorf("pinned model: %s %s %v", b, m, err)
	}
	if b, m, err := Resolve("codex", ""); err != nil || b != "codex" || m != "" {
		t.Errorf("a named backend changed: %s %s %v", b, m, err)
	}
}
