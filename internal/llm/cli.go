package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CLIs run in an empty temporary directory, so project files and
// instructions (CLAUDE.md, AGENTS.md) never leak into the translation.
func run(ctx context.Context, name string, args []string, stdin string) (string, string, error) {
	dir, err := os.MkdirTemp("", "stele-llm-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, name, args...)
	containProcess(cmd)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Start()
	if err == nil {
		untrack := track(cmd)
		err = cmd.Wait()
		untrack()
	}
	if err != nil {
		if os.Getenv("CODEX_SANDBOX") != "" && ctx.Err() == nil {
			// Inside Codex's sandbox a model CLI cannot start (codex: "failed
			// to initialize in-process app-server client: Operation not
			// permitted") and has no network: say what to do once, instead
			// of retrying every statement (2026-11-06).
			return stdout.String(), stderr.String(), fmt.Errorf("%w (%s: %v)", ErrSandboxed, name, err)
		}
		return stdout.String(), stderr.String(), fmt.Errorf("%s: %w\n%s", name, err, tail(stderr.String()+stdout.String()))
	}
	return stdout.String(), stderr.String(), nil
}

// ErrSandboxed is a model call that failed inside Codex's sandbox, where
// no model CLI can run.
var ErrSandboxed = errors.New("Stele calls your model through its CLI, which cannot run inside Codex's sandbox: run this stele command again outside the sandbox. Once Codex trusts this project, its rule .codex/rules/stele.rules lets `stele change` and `stele adopt` run outside the sandbox without asking; otherwise approve running it outside the sandbox")

func tail(s string) string {
	if len(s) > 2000 {
		return "..." + s[len(s)-2000:]
	}
	return s
}

// claudeCLI runs Claude Code in print mode with no tools, the request's system
// prompt in place of Claude Code's own, and native JSON-schema output.
type claudeCLI struct{ model, effort string }

func (c *claudeCLI) Name() string { return "claude-cli:" + orDefault(c.model) + withEffort(c.effort) }

func (c *claudeCLI) Complete(ctx context.Context, req Request) (*Reply, error) {
	args := []string{"-p", "--output-format", "json", "--json-schema", req.Schema,
		"--system-prompt", systemOf(req), "--tools", "", "--no-session-persistence"}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if c.effort != "" {
		args = append(args, "--effort", c.effort)
	}
	out, _, err := run(ctx, "claude", args, renderPrompt(req, false, false))
	if err != nil {
		return nil, err
	}
	var env struct {
		IsError          bool            `json:"is_error"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
		TotalCostUSD     float64         `json:"total_cost_usd"`
		Usage            struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			Output     int64 `json:"output_tokens"`
		} `json:"usage"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return nil, fmt.Errorf("claude: unreadable output: %w\n%s", err, tail(out))
	}
	if env.IsError {
		return nil, fmt.Errorf("claude: %s", env.Result)
	}
	text := string(env.StructuredOutput)
	if len(env.StructuredOutput) == 0 || text == "null" {
		if text, err = ExtractJSON(env.Result); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
	}
	r := &Reply{JSON: text, Usage: Usage{
		InputTokens: env.Usage.Input, CacheWriteTokens: env.Usage.CacheWrite,
		CacheReadTokens: env.Usage.CacheRead, OutputTokens: env.Usage.Output, CostUSD: env.TotalCostUSD,
	}}
	for m := range env.ModelUsage {
		r.Model = m // the model that answered, e.g. after a fallback
	}
	return r, nil
}

// codexCLI runs `codex exec` read-only with native output-schema support.
// Codex has no system-prompt flag, so the system prompt leads the prompt.
type codexCLI struct{ model, effort string }

func (c *codexCLI) Name() string { return "codex-cli:" + orDefault(c.model) + withEffort(c.effort) }

// HedgeAfter: a stalled Codex call is sent again (Hedger).
func (c *codexCLI) HedgeAfter() time.Duration { return codexHedgeAfter }

// codexHedgeAfter is how long a Codex call runs before the same request is
// sent again. A few calls stall: in a developer trial at medium effort, 2 of
// 8 ran into the 3-minute request limit, and the same request sent again
// answered in 30 to 45 seconds; most calls answer within 90 seconds.
const codexHedgeAfter = 100 * time.Second

// Hedger is a client whose calls sometimes stall: a call that has not
// answered after HedgeAfter is sent again (Hedge).
type Hedger interface{ HedgeAfter() time.Duration }

// Hedge runs call, and once more if it has not answered after the given
// time; the first answer is used and the other call is cancelled. A call
// that fails before then fails at once: retrying is the caller's. Each run
// of call sets its own time limit, so the second attempt gets a full one.
func Hedge(ctx context.Context, after time.Duration, call func(context.Context) (*Reply, error)) (*Reply, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type answer struct {
		r   *Reply
		err error
	}
	answers := make(chan answer, 2)
	start := func() {
		go func() {
			r, err := call(ctx)
			answers <- answer{r, err}
		}()
	}
	start()
	timer := time.NewTimer(after)
	defer timer.Stop()
	running, hedged := 1, false
	var firstErr error
	for {
		select {
		case <-timer.C:
			if !hedged {
				hedged = true
				running++
				start()
			}
		case a := <-answers:
			running--
			if a.err == nil {
				return a.r, nil
			}
			if firstErr == nil {
				firstErr = a.err
			}
			if running == 0 {
				return nil, firstErr
			}
		}
	}
}

func (c *codexCLI) Complete(ctx context.Context, req Request) (*Reply, error) {
	dir, err := os.MkdirTemp("", "stele-codex-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	schema, last := filepath.Join(dir, "schema.json"), filepath.Join(dir, "last.json")
	if err := os.WriteFile(schema, []byte(req.Schema), 0o644); err != nil {
		return nil, err
	}
	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--sandbox", "read-only",
		"--output-schema", schema, "-o", last}
	if c.model != "" {
		args = append(args, "-m", c.model)
	}
	if c.effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+c.effort)
	}
	args = append(args, "-") // read the prompt from stdin
	if _, _, err := run(ctx, "codex", args, renderPrompt(req, true, false)); err != nil {
		return nil, err
	}
	out, err := os.ReadFile(last)
	if err != nil {
		return nil, fmt.Errorf("codex wrote no final message: %w", err)
	}
	text, err := ExtractJSON(string(out))
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	return &Reply{JSON: text}, nil // codex exec does not report usage in this mode
}

// commandCLI runs any shell command: the full prompt, schema included, goes
// to stdin and the reply is read from stdout.
type commandCLI struct{ command string }

func (c *commandCLI) Name() string { return "cmd:" + c.command }

func (c *commandCLI) Complete(ctx context.Context, req Request) (*Reply, error) {
	out, _, err := run(ctx, "sh", []string{"-c", c.command}, renderPrompt(req, true, true))
	if err != nil {
		return nil, err
	}
	text, err := ExtractJSON(out)
	if err != nil {
		return nil, err
	}
	return &Reply{JSON: text}, nil
}

// withEffort marks a non-default reasoning effort in a backend name, so
// reports and journals show which setting produced a run.
func withEffort(effort string) string {
	if effort == "" {
		return ""
	}
	return "@" + effort
}

func orDefault(model string) string {
	if model == "" {
		return "default"
	}
	return model
}
