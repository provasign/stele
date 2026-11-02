// Package llm is the one seam between Stele and a language model. A Client is
// either a model API with a key (Anthropic, or OpenAI and compatible servers)
// or a coding-agent CLI the user already runs: Claude Code, Codex, or any
// command that reads a prompt on stdin and prints the reply.
//
// Every backend gets the same request: a system prompt, a transcript and a
// JSON schema for the reply. Backends with native structured output use it;
// the rest are told the schema and their reply is parsed leniently. Callers
// validate the content themselves either way.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Turn is one message of the transcript.
type Turn struct {
	Role string // "user" or "assistant"
	Text string
}

// Request is one completion.
type Request struct {
	System string
	Turns  []Turn
	Schema string // JSON schema the reply must match
}

// Usage is what a reply cost. CLIs report less than the API; zero means unknown.
type Usage struct {
	InputTokens      int64   `json:"input_tokens,omitempty"`
	CacheWriteTokens int64   `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens  int64   `json:"cache_read_input_tokens,omitempty"`
	OutputTokens     int64   `json:"output_tokens,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.CacheWriteTokens += u2.CacheWriteTokens
	u.CacheReadTokens += u2.CacheReadTokens
	u.OutputTokens += u2.OutputTokens
	u.CostUSD += u2.CostUSD
}

// Reply is the model's answer: JSON text matching the request schema.
type Reply struct {
	JSON  string
	Model string // as reported by the backend, when it reports one
	Usage Usage
}

// Client completes requests.
type Client interface {
	Complete(ctx context.Context, req Request) (*Reply, error)
	// Name identifies the backend and model for reports, e.g. "anthropic:claude-opus-5-5".
	Name() string
}

// Config selects a backend.
type Config struct {
	Backend string // anthropic | openai | claude | codex | cmd
	Model   string // passed to the backend; empty uses the backend's default
	Command string // for cmd: shell command that reads the prompt on stdin
	// Effort is the reasoning effort: low | medium | high | xhigh | max for
	// anthropic and claude; passed to codex as model_reasoning_effort.
	Effort string
}

// New returns the configured client.
func New(cfg Config) (Client, error) {
	// A command given for another backend would be ignored and the run
	// would reach a model it was meant to avoid.
	if cfg.Command != "" && cfg.Backend != "cmd" {
		return nil, fmt.Errorf("--llm-cmd is used only by backend cmd, not %s: pass --llm cmd or drop --llm-cmd", cfg.Backend)
	}
	switch cfg.Backend {
	case "anthropic":
		return newAPI(cfg), nil
	case "openai":
		return newOpenAI(cfg)
	case "claude":
		return &claudeCLI{model: cfg.Model, effort: cfg.Effort}, nil
	case "codex":
		return &codexCLI{model: cfg.Model, effort: cfg.Effort}, nil
	case "cmd":
		if cfg.Command == "" {
			return nil, fmt.Errorf("backend cmd needs --llm-cmd, a shell command that reads the prompt on stdin and prints the reply")
		}
		return &commandCLI{command: cfg.Command}, nil
	}
	return nil, fmt.Errorf("unknown LLM backend %q (anthropic, openai, claude, codex, cmd)", cfg.Backend)
}

// renderPrompt flattens a request into one prompt for backends that take a
// single text input. withSystem includes the system prompt for backends with
// no system-prompt option; withSchema spells out the schema for backends with
// no structured-output option.
func renderPrompt(req Request, withSystem, withSchema bool) string {
	var b strings.Builder
	if withSystem {
		b.WriteString(req.System)
		b.WriteString("\n\n---\n\n")
	}
	if len(req.Turns) == 1 {
		b.WriteString(req.Turns[0].Text)
	} else {
		for i, t := range req.Turns {
			switch {
			case t.Role == "assistant":
				b.WriteString("Your earlier reply:\n\n" + t.Text + "\n\n")
			case i == 0:
				b.WriteString(t.Text + "\n\n")
			default:
				b.WriteString("Then the user wrote:\n\n" + t.Text + "\n\n")
			}
		}
	}
	if withSchema {
		b.WriteString("\n\nReply with a single JSON object and nothing else. It must match this JSON schema:\n\n" + req.Schema + "\n")
	}
	return b.String()
}

// ExtractJSON returns the first top-level JSON object in s, tolerating prose
// and markdown fences around it.
func ExtractJSON(s string) (string, error) {
	start := strings.IndexByte(s, '{')
	for start >= 0 {
		dec := json.NewDecoder(strings.NewReader(s[start:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == nil {
			return string(raw), nil
		}
		next := strings.IndexByte(s[start+1:], '{')
		if next < 0 {
			break
		}
		start += 1 + next
	}
	return "", fmt.Errorf("no JSON object in reply: %.300s", s)
}
