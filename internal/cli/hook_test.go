package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var (
	hookBinOnce sync.Once
	hookBin     string
	hookBinErr  error
)

// steleBinary builds Stele once for the hook tests: the hook runs Stele
// itself for the rules, the drift and the touches.
func steleBinary(t *testing.T) string {
	t.Helper()
	hookBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "stele-hook-bin")
		if err != nil {
			hookBinErr = err
			return
		}
		hookBin = filepath.Join(dir, "stele")
		out, err := exec.Command("go", "build", "-o", hookBin, "github.com/provasign/stele/cmd/stele").CombinedOutput()
		if err != nil {
			hookBinErr = &buildError{string(out)}
		}
	})
	if hookBinErr != nil {
		t.Fatal(hookBinErr)
	}
	return hookBin
}

type buildError struct{ out string }

func (e *buildError) Error() string { return "go build: " + e.out }

// hookFixture is a project with one approved requirement, REQ-1: the door
// MUST be locked, in specs/door.md.
type hookFixture struct {
	project, registry, file, original string
}

func newHookFixture(t *testing.T) hookFixture {
	t.Helper()
	project := t.TempDir()
	specs, registry := filepath.Join(project, "specs"), filepath.Join(project, "registry")
	os.MkdirAll(specs, 0o755)
	os.MkdirAll(registry, 0o755)
	f := hookFixture{project: project, registry: registry, file: filepath.Join(specs, "door.md"), original: "# Door\n\nREQ-1: The door MUST be locked.\n"}
	prose := "Section: Door\nREQ-1: The door MUST be locked."
	sum := sha256.Sum256([]byte(prose))
	hash := hex.EncodeToString(sum[:])[:16]
	id := "door.md#REQ-1"
	statements, _ := json.Marshal([]map[string]any{{"id": id, "source": "door.md#L3", "prose": prose, "reference": []string{}}})
	source, _ := json.Marshal(map[string]any{"statementId": id, "doc": "door.md#L3", "textHash": hash, "statementHash": hash, "prose": prose})
	config, _ := json.Marshal(map[string]any{"formatVersion": 1, "registry": "registry", "translate": map[string]any{"llm": "claude", "drafts": 1},
		"specs": map[string]any{"root": "specs", "format": "markdown", "snapshot": "statements.yaml"}})
	for path, content := range map[string]string{
		f.file: f.original,
		filepath.Join(project, "statements.yaml"): string(statements),
		filepath.Join(registry, "context.yaml"):   "context: c\nformatVersion: 1\n---\nentity: Door\ncontext: c\nattributes:\n  locked: {type: bool}\n",
		filepath.Join(registry, "rules.yaml"):     "id: R1\nkind: invariant\nassert: door.locked\nsource: " + string(source) + "\n",
		filepath.Join(project, "stele.yaml"):      string(config),
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// runHook runs `stele hook` on one event and returns its answer (nil when
// it says nothing).
func runHook(t *testing.T, input map[string]any, args ...string) map[string]any {
	t.Helper()
	src, _ := json.Marshal(input)
	var out bytes.Buffer
	if code := cmdHook(args, bytes.NewReader(src), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out.String()) == "" {
		return nil
	}
	var answer struct {
		Out map[string]any `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return answer.Out
}

func hookEnv(t *testing.T, f hookFixture) {
	t.Helper()
	t.Setenv("STELE_BIN", steleBinary(t))
	cache := filepath.Join(f.project, "cache")
	os.MkdirAll(cache, 0o755)
	t.Setenv("STELE_HOOK_CACHE", cache)
	t.Setenv("STELE_APPROVAL_PROMPT", "")
}

func hookContext(answer map[string]any) string {
	if answer == nil {
		return ""
	}
	s, _ := answer["additionalContext"].(string)
	return s
}

// TestHookDriftOnce: after an edit the hook reports drift once, in the
// project's words, and reports recovery; no model is called.
func TestHookDriftOnce(t *testing.T) {
	f := newHookFixture(t)
	hookEnv(t, f)
	invoke := func() map[string]any {
		return runHook(t, map[string]any{"hook_event_name": "PostToolUse", "cwd": f.project, "session_id": "integration-test"})
	}
	if got := invoke(); got != nil {
		t.Fatalf("in sync, the hook said: %v", got)
	}
	os.WriteFile(f.file, []byte(strings.Replace(f.original, "locked", "unlocked", 1)), 0o644)
	answer := invoke()
	note := hookContext(answer)
	if answer["hookEventName"] != "PostToolUse" {
		t.Errorf("event: %v", answer)
	}
	for _, want := range []string{
		`the specs differ from what the owner approved \(requirements: 1 new or edited, 1 approved for text that has since changed\)`,
		`- door\.md#REQ-1: edited since the owner approved it \(specs/door\.md:3\)`,
		"Next: run `stele change markdown \\. --json`",
	} {
		if !regexp.MustCompile(want).MatchString(note) {
			t.Errorf("missing %q in:\n%s", want, note)
		}
	}
	if strings.Contains(note, f.project) || regexp.MustCompile(`spec\.changed|rule\.stale|provenance|stable rule IDs`).MatchString(note) {
		t.Errorf("not in the project's words:\n%s", note)
	}
	if got := invoke(); got != nil {
		t.Errorf("unchanged drift said again: %v", got)
	}
	os.WriteFile(f.file, []byte(f.original), 0o644)
	if note := hookContext(invoke()); !strings.Contains(note, "match what the owner approved again") {
		t.Errorf("recovery: %q", note)
	}
	if got := invoke(); got != nil {
		t.Errorf("after recovery: %v", got)
	}
}

// TestHookPrompt: a request that touches an approved requirement gets it
// in the agent's context; an unrelated request gets nothing.
func TestHookPrompt(t *testing.T) {
	f := newHookFixture(t)
	hookEnv(t, f)
	ask := func(prompt string) map[string]any {
		return runHook(t, map[string]any{"hook_event_name": "UserPromptSubmit", "cwd": f.project, "session_id": "integration-test", "prompt": prompt})
	}
	answer := ask("Change it so the door is not locked at night, for the cleaners")
	note := hookContext(answer)
	if answer["hookEventName"] != "UserPromptSubmit" || !strings.Contains(note, "- REQ-1: The door MUST be locked. (door.md#L3)") || !strings.Contains(note, "ask the owner") {
		t.Errorf("prompt hook: %v", answer)
	}
	if got := ask("Bump all dependencies to their latest versions"); got != nil {
		t.Errorf("an unrelated request got: %v", got)
	}
}

// TestHookTouches: a code edit that touches an approved requirement is
// named once.
func TestHookTouches(t *testing.T) {
	f := newHookFixture(t)
	hookEnv(t, f)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", f.project, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(f.project, ".gitignore"), []byte("cache/\n"), 0o644)
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	invoke := func() map[string]any {
		return runHook(t, map[string]any{"hook_event_name": "PostToolUse", "cwd": f.project, "session_id": "touch-test"})
	}
	if got := invoke(); got != nil {
		t.Fatalf("before the edit: %v", got)
	}
	os.WriteFile(filepath.Join(f.project, "door.go"), []byte("package door\n\ntype Door struct{ Locked bool }\n\nfunc Open(d *Door) { d.Locked = false }\n"), 0o644)
	note := hookContext(invoke())
	if !strings.Contains(note, "touches 1 approved requirement") || !strings.Contains(note, "REQ-1: The door MUST be locked") ||
		!regexp.MustCompile(`reads or writes locked|is locked`).MatchString(note) {
		t.Errorf("touches: %q", note)
	}
	if got := invoke(); got != nil {
		t.Errorf("the same touched requirements said again: %v", got)
	}
}

// TestHookWaitingRequests: a kept request the owner has not approved is
// named once; with many waiting, this session's by name and the rest as a
// count. A request not kept is a task: no reminder.
func TestHookWaitingRequests(t *testing.T) {
	f := newHookFixture(t)
	hookEnv(t, f)
	config, _ := json.Marshal(map[string]any{"formatVersion": 1, "registry": "registry", "translate": map[string]any{"llm": "claude", "drafts": 1},
		"specs": map[string]any{"root": ".", "format": "shale", "snapshot": "statements.yaml"}})
	write := func(rel, content string) {
		p := filepath.Join(f.project, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stele.yaml", string(config))
	write("statements.yaml", "[]")
	write("registry/rules.yaml", "")
	write(".shale/s1.yaml", "id: s1\nintent:\n    title: Lock the door at night\n")
	keep := func(ids ...string) {
		var b strings.Builder
		for _, id := range ids {
			b.WriteString("- id: intent/" + id + "\n  title: t\n  by: agent\n  at: \"2026-11-09\"\n")
		}
		write(".stele/kept-intents.yaml", b.String())
	}
	invoke := func() string {
		return hookContext(runHook(t, map[string]any{"hook_event_name": "PostToolUse", "cwd": f.project, "session_id": "request-test"}))
	}
	if got := invoke(); got != "" {
		t.Errorf("a request not kept got a reminder: %q", got)
	}
	keep("s1")
	note := invoke()
	if !strings.Contains(note, `not approved by the owner yet: "Lock the door at night"`) || strings.Contains(note, "run `run ") {
		t.Errorf("reminder: %q", note)
	}
	if again := invoke(); strings.Contains(again, "not approved by the owner") {
		t.Errorf("the reminder repeated: %q", again)
	}
	for i := 2; i <= 6; i++ {
		n := string(rune('0' + i))
		write(".shale/old"+n+".yaml", "id: old"+n+"\nintent:\n    title: Earlier request "+n+"\n")
	}
	write(".shale/request-test.yaml", "id: request-test\nintent:\n    title: Open the gate at dawn\n")
	keep("s1", "old2", "old3", "old4", "old5", "old6", "request-test")
	many := invoke()
	if !strings.Contains(many, `not approved by the owner yet: "Open the gate at dawn" (and 6 others)`) || strings.Contains(many, "Earlier request") {
		t.Errorf("many waiting: %q", many)
	}
}

// TestHookRecordsAgent: the agent and the model in use, for Stele's own
// calls; a model stands until the same session names another.
func TestHookRecordsAgent(t *testing.T) {
	f := newHookFixture(t)
	hookEnv(t, f)
	record := func() agentRecord {
		var r agentRecord
		src, _ := os.ReadFile(filepath.Join(f.project, ".stele", "cache", "agent.json"))
		json.Unmarshal(src, &r)
		return r
	}
	event := func(fields map[string]any, args ...string) map[string]any {
		input := map[string]any{"cwd": f.project, "session_id": "s1"}
		for k, v := range fields {
			input[k] = v
		}
		return runHook(t, input, args...)
	}
	if got := event(map[string]any{"hook_event_name": "SessionStart", "source": "startup", "model": "claude-opus-5-5"}); got != nil {
		t.Errorf("session start said: %v", got)
	}
	if got := record(); got != (agentRecord{"claude", "claude-opus-5-5", "s1"}) {
		t.Errorf("record: %+v", got)
	}
	event(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_input": map[string]any{}})
	if record().Model != "claude-opus-5-5" {
		t.Error("an event without a model forgot it")
	}
	event(map[string]any{"hook_event_name": "PostModelSwitch", "from_model": "claude-opus-5-5", "to_model": "claude-sonnet-5-5"})
	if record().Model != "claude-sonnet-5-5" {
		t.Errorf("model switch: %+v", record())
	}
	event(map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s2"})
	if got := record(); got != (agentRecord{"claude", "", "s2"}) {
		t.Errorf("a new session kept the old model: %+v", got)
	}
	event(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s", "model": "gpt-5.5", "turn_id": "t", "prompt": "hello"}, "--agent", "codex")
	if got := record(); got != (agentRecord{"codex", "gpt-5.5", "s"}) {
		t.Errorf("codex: %+v", got)
	}
}

// TestHookCodexFromSubdirectory: Codex runs the hook from wherever the
// session is, with Claude Code's fields and more; the hook finds the
// project and answers in the same format.
func TestHookCodexFromSubdirectory(t *testing.T) {
	f := newHookFixture(t)
	hookEnv(t, f)
	sub := filepath.Join(f.project, "src", "deep")
	os.MkdirAll(sub, 0o755)
	run := func(event string, extra map[string]any) map[string]any {
		input := map[string]any{"hook_event_name": event, "cwd": sub, "session_id": "s", "model": "m", "turn_id": "t", "permission_mode": "default", "transcript_path": nil}
		for k, v := range extra {
			input[k] = v
		}
		return runHook(t, input, "--agent", "codex")
	}
	prompt := run("UserPromptSubmit", map[string]any{"prompt": "Change it so the door is not locked at night, for the cleaners"})
	if prompt["hookEventName"] != "UserPromptSubmit" || !strings.Contains(hookContext(prompt), "The door MUST be locked") {
		t.Errorf("prompt: %v", prompt)
	}
	if got := run("UserPromptSubmit", map[string]any{"prompt": "Bump all dependencies to their latest versions"}); got != nil {
		t.Errorf("unrelated: %v", got)
	}
	tool := map[string]any{"tool_name": "apply_patch", "tool_input": map[string]any{}, "tool_response": map[string]any{}, "tool_use_id": "u"}
	if got := run("PostToolUse", tool); got != nil {
		t.Errorf("in sync: %v", got)
	}
	os.WriteFile(f.file, []byte(strings.Replace(f.original, "locked", "unlocked", 1)), 0o644)
	drift := run("PostToolUse", tool)
	if drift["hookEventName"] != "PostToolUse" || !strings.Contains(hookContext(drift), "1 approved for text that has since changed") {
		t.Errorf("drift: %v", drift)
	}
}

// TestHookAsksBeforeApproval: a command that records an approval asks the
// owner in Claude Code's prompt, with the check's summary; where Claude
// Code cannot ask, the agent asks in chat and adds --asked.
func TestHookAsksBeforeApproval(t *testing.T) {
	f := newHookFixture(t)
	hookEnv(t, f)
	run := func(command string, extra map[string]any) map[string]any {
		input := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_input": map[string]any{"command": command},
			"cwd": f.project, "session_id": "s", "permission_mode": "default"}
		for k, v := range extra {
			input[k] = v
		}
		return runHook(t, input)
	}
	work := filepath.Join(f.project, ".stele", "change", "add-lock")
	os.MkdirAll(work, 0o755)
	result, _ := json.Marshal(map[string]any{"change": "add-lock", "added": []string{"x"}, "statements": map[string]string{"x": "The door MUST be locked at night."}, "registry": f.registry})
	os.WriteFile(filepath.Join(work, "result.json"), result, 0o644)
	config := filepath.Join(f.project, "stele.yaml")
	setFormat := func(from, to string) {
		src, _ := os.ReadFile(config)
		os.WriteFile(config, []byte(strings.Replace(string(src), `"`+from+`"`, `"`+to+`"`, 1)), 0o644)
	}
	// A spec tool names its changes, and the prompt says which one.
	setFormat("markdown", "openspec")
	asked := run("stele change answer add-lock approve-1a2b approve", nil)
	reason, _ := asked["permissionDecisionReason"].(string)
	if asked["hookEventName"] != "PreToolUse" || asked["permissionDecision"] != "ask" ||
		!strings.Contains(reason, `records your approval of "add-lock" as the owner`) || !strings.Contains(reason, "Stele read 1 requirement.") {
		t.Errorf("asked: %v", asked)
	}
	if regexp.MustCompile(`Agent|--asked`).MatchString(reason) {
		t.Errorf("the prompt carries a note to the agent: %q", reason)
	}
	// A change with no saved check still asks, without the summary.
	for _, command := range []string{"cd . && ./bin/stele change answer other q1 approve --by Ana", "stele approve --registry registry --all --by Ana", "(stele approve)"} {
		if got := run(command, nil); got == nil || got["permissionDecision"] != "ask" {
			t.Errorf("%s: %v", command, got)
		}
	}
	// Every other command passes without a word.
	for _, command := range []string{"stele change answer add-lock q1 show", "stele change markdown . --json", "stele approve --registry registry --list",
		`git commit -m "stele approve flow"`, "echo stele change answer a b approve", "ls", "stele approver", "stele approve; ls --list"} {
		got := run(command, nil)
		if command == "stele approve; ls --list" {
			if got == nil {
				t.Errorf("%s passed: --list belongs to another command", command)
			}
			continue
		}
		if got != nil {
			t.Errorf("%s: %v", command, got)
		}
	}
	if got := run("stele change answer add-lock q approve", map[string]any{"tool_name": "Read"}); got != nil {
		t.Errorf("not a shell command: %v", got)
	}
	// Where Claude Code cannot ask, the agent asks in chat and says so.
	denied := run("stele change answer add-lock q approve", map[string]any{"permission_mode": "bypassPermissions"})
	if denied["permissionDecision"] != "deny" || !regexp.MustCompile(`(?s)ask them the approval question in chat.*--asked`).MatchString(denied["permissionDecisionReason"].(string)) {
		t.Errorf("denied: %v", denied)
	}
	if got := run("stele change answer add-lock q approve --asked", map[string]any{"permission_mode": "bypassPermissions"}); got != nil {
		t.Errorf("--asked held: %v", got)
	}
	if got := run("stele change answer add-lock q approve", map[string]any{"permission_mode": "dontAsk"}); got["permissionDecision"] != "deny" {
		t.Errorf("dontAsk: %v", got)
	}
	// The owner's yes in chat passes in any mode.
	if got := run("stele change answer add-lock q approve --asked", nil); got != nil {
		t.Errorf("--asked: %v", got)
	}
	// A requirements/ folder's change is named after its branch: the prompt leaves the name out.
	setFormat("openspec", "markdown")
	if r, _ := run("stele change answer add-lock approve-1a2b approve", nil)["permissionDecisionReason"].(string); !regexp.MustCompile(`records your approval as the owner.*Stele read 1 requirement\.`).MatchString(r) {
		t.Errorf("unnamed: %q", r)
	}
	// An unattended run turns the prompt off where Claude Code starts.
	t.Setenv("STELE_APPROVAL_PROMPT", "off")
	if got := run("stele change answer add-lock q approve", nil); got != nil {
		t.Errorf("prompt off: %v", got)
	}
}

// TestHookOutsideProject: no stele.yaml up the tree, nothing said.
func TestHookOutsideProject(t *testing.T) {
	if got := runHook(t, map[string]any{"hook_event_name": "PostToolUse", "cwd": t.TempDir()}); got != nil {
		t.Errorf("outside a project: %v", got)
	}
}
