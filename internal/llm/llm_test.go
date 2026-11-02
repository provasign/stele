package llm

import (
	"context"
	"strings"
	"testing"
)

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"rules": []}`: `{"rules": []}`,
		"Here you go:\n```json\n{\"a\": {\"b\": 1}}\n```": `{"a": {"b": 1}}`,
		"{not json} then {\"ok\": true}":                  `{"ok": true}`,
	}
	for in, want := range cases {
		got, err := ExtractJSON(in)
		if err != nil || got != want {
			t.Errorf("ExtractJSON(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ExtractJSON("no object here"); err == nil {
		t.Error("want an error when there is no JSON object")
	}
}

func TestRenderPrompt(t *testing.T) {
	req := Request{System: "SYS", Schema: `{"type":"object"}`, Turns: []Turn{
		{Role: "user", Text: "translate"}, {Role: "assistant", Text: `{"rules":[]}`}, {Role: "user", Text: "fix it"},
	}}
	p := renderPrompt(req, true, true)
	for _, want := range []string{"SYS", "translate", `Your earlier reply:`, `{"rules":[]}`, "Then the user wrote:", "fix it", `{"type":"object"}`} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if p := renderPrompt(req, false, false); strings.Contains(p, "SYS") || strings.Contains(p, "JSON schema") {
		t.Errorf("system prompt or schema included when the backend takes them natively:\n%s", p)
	}
}

// The cmd backend sends the whole prompt on stdin and reads JSON from stdout.
func TestCommandBackend(t *testing.T) {
	c, err := New(Config{Backend: "cmd", Command: `grep -q 'stdin-marker' && echo 'model says: {"rules": []}'`})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Complete(context.Background(), Request{System: "s", Schema: "{}", Turns: []Turn{{Role: "user", Text: "stdin-marker"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.JSON != `{"rules": []}` {
		t.Errorf("JSON = %q", r.JSON)
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	for _, cfg := range []Config{{Backend: "cmd"}, {Backend: "openai"}, {Backend: "openai", Model: "m"}, {Backend: "gpt"}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded, want an error", cfg)
		}
	}
}

func TestCommandOnlyWithCmdBackend(t *testing.T) {
	if _, err := New(Config{Backend: "anthropic", Command: "false"}); err == nil {
		t.Error("a command with another backend must be refused, not ignored")
	}
	if _, err := New(Config{Backend: "cmd", Command: "false"}); err != nil {
		t.Error(err)
	}
}
