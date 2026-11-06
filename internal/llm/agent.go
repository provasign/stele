package llm

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
)

// AgentRecordFile is where the agent hook records the coding agent and
// model in use, under the project's .stele directory: the hook writes it on
// every prompt and model switch, so it names what the developer is using
// at that moment.
const AgentRecordFile = ".stele/cache/agent.json"

// AgentRecord is the hook's record.
type AgentRecord struct {
	Agent   string `json:"agent"` // claude | codex
	Model   string `json:"model,omitempty"`
	Session string `json:"session,omitempty"`
}

// ResolveAgent is the backend and model the backend `agent` stands for: the
// coding agent Stele is run from, and the model it is using, so that
// Stele's calls land on the CLI and model the developer has chosen at
// that moment (the owner, 2026-11-06). It is Claude Code when its
// environment says so (CLAUDECODE is set in every command it runs); else
// the agent the hook last recorded under dir's project; else, outside any
// agent (CI, a shell), the claude or codex CLI on PATH, else the API an
// API key is set for. The model is the record's when the record names the
// agent found, else empty: the CLI's own default. Codex puts the active
// model in every hook event; Claude Code names it at session start and on
// each model switch. ok is false when no backend was found.
func ResolveAgent(dir string) (backend, model string, ok bool) {
	rec := readAgentRecord(dir)
	switch {
	case os.Getenv("CLAUDECODE") != "":
		backend = "claude"
	case rec != nil && (rec.Agent == "claude" || rec.Agent == "codex"):
		backend = rec.Agent
	default:
		for _, cli := range []string{"claude", "codex"} {
			if _, err := exec.LookPath(cli); err == nil {
				backend = cli
				break
			}
		}
		if backend == "" {
			switch {
			case os.Getenv("ANTHROPIC_API_KEY") != "":
				return "anthropic", "", true
			case os.Getenv("OPENAI_API_KEY") != "":
				return "openai", "", true
			}
			return "", "", false
		}
	}
	if rec != nil && rec.Agent == backend {
		model = rec.Model
	}
	return backend, model, true
}

// readAgentRecord is the hook's record under the project dir is in (the
// directory holding stele.yaml, found from dir upwards), or nil.
func readAgentRecord(dir string) *AgentRecord {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "stele.yaml")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
	src, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(AgentRecordFile)))
	if err != nil {
		return nil
	}
	var rec AgentRecord
	if json.Unmarshal(src, &rec) != nil {
		return nil
	}
	return &rec
}
