package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// cmdHook is the coding agent's hook: Claude Code and Codex run `stele hook`
// on their events, whose input and output share one format, so the hooks
// need nothing but Stele's own binary.
// UserPromptSubmit: the approved requirements the request touches, for the
// plan to keep. PreToolUse (Bash): a command that records the owner's
// approval asks the owner, in Claude Code's own permission prompt.
// PostToolUse: saved-spec drift, and the approved requirements the code
// change touches. SessionStart and PostModelSwitch: the agent and model in
// use, for Stele's own calls.
// No shell interpolation, model invocation or rule writes. It always exits
// 0: a hook that fails never blocks the agent.
func cmdHook(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "claude", "the coding agent running the hook (claude)")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	h := &hook{agent: *agent, out: out, event: "PostToolUse"}
	if err := h.run(in); err != nil && h.event != "UserPromptSubmit" && h.event != "PreToolUse" {
		// Prevention stays silent when it cannot answer, and a failed guard
		// never blocks a command; after an edit the agent hears it.
		h.say("PostToolUse", map[string]any{"additionalContext": fmt.Sprintf(
			"Stele's hook failed (%v), so it is not known whether the specs match what the owner approved.", err)})
	}
	return 0
}

type hook struct {
	agent, event string
	out          io.Writer
}

type hookInput struct {
	Event          string `json:"hook_event_name"`
	Cwd            string `json:"cwd"`
	Session        string `json:"session_id"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	ToModel        string `json:"to_model"`
	ToolName       string `json:"tool_name"`
	PermissionMode string `json:"permission_mode"`
	ToolInput      struct {
		Command any `json:"command"`
	} `json:"tool_input"`
}

// say writes one hook answer in the agents' shared format, as written.
func (h *hook) say(event string, fields map[string]any) {
	fields["hookEventName"] = event
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(map[string]any{"hookSpecificOutput": fields}) == nil {
		h.out.Write(b.Bytes())
	}
}

func (h *hook) run(in io.Reader) error {
	src, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	var input hookInput
	if err := json.Unmarshal(src, &input); err != nil {
		return err
	}
	if input.Event != "" {
		h.event = input.Event
	}
	project := input.Cwd
	if project == "" {
		if project, err = os.Getwd(); err != nil {
			return err
		}
	}
	if project, err = filepath.Abs(project); err != nil {
		return err
	}
	for !exists(filepath.Join(project, "stele.yaml")) {
		parent := filepath.Dir(project)
		if parent == project {
			return nil // not a Stele project
		}
		project = parent
	}
	if h.event == "PreToolUse" {
		h.approval(project, input)
		return nil
	}
	h.recordAgent(project, input)
	switch h.event {
	case "SessionStart", "PostModelSwitch":
		return nil
	case "UserPromptSubmit":
		// Prevention, never blocking: say nothing when the request touches
		// no approved requirement, or when Stele cannot answer.
		r := steleRun(project, 5*time.Second, input.Prompt, "rules", "--project", project)
		if r.err == nil && strings.TrimSpace(r.stdout) != "" {
			h.say("UserPromptSubmit", map[string]any{"additionalContext": strings.TrimSpace(r.stdout)})
		}
		return nil
	}
	return h.afterTool(project, input)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// The agent this hook runs in and the model it is using, recorded for
// Stele's own calls (translate.llm: agent) under the project. Codex names
// the model in every event; Claude Code at session start (not always) and
// on a model switch, so the model stands until the same session names
// another.
type agentRecord struct {
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	Session string `json:"session"`
}

func (h *hook) recordAgent(project string, input hookInput) {
	file := filepath.Join(project, ".stele", "cache", "agent.json")
	var previous agentRecord
	if src, err := os.ReadFile(file); err == nil {
		json.Unmarshal(src, &previous)
	}
	kept := ""
	if previous.Agent == h.agent && previous.Session == input.Session {
		kept = previous.Model
	}
	record := agentRecord{Agent: h.agent, Model: firstNonEmpty(input.ToModel, input.Model, kept), Session: input.Session}
	if record == previous {
		return
	}
	b, _ := json.Marshal(record)
	// A read-only checkout: Stele falls back to the CLI it finds.
	if os.MkdirAll(filepath.Dir(file), 0o755) == nil {
		os.WriteFile(file, append(b, '\n'), 0o644)
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// A command that records an approval: `stele change answer <change>
// <question> approve`, or `stele approve` (not its --list). The owner
// approves, never the agent, and an instruction alone did not hold an eager
// agent back. This catches the command as Stele's instructions give it; it
// is a guard against a mistake, not a security boundary. A command counts
// where one starts, so a commit message that names one is not one.
const approvalStart = `(?:^|[;&|(\n])\s*(?:\w+=\S*\s+)*(?:\S*/)?stele\s+`

var (
	approvalAnswer = regexp.MustCompile(approvalStart + `change\s+answer\s+(\S+)\s+\S+\s+approve(?:$|[\s;&|)])`)
	approvalDirect = regexp.MustCompile(approvalStart + `approve((?:[\s)][^;&|]*)?)(?:$|[;&|])`)
	approvalList   = regexp.MustCompile(`(^|\s)--?list\b`)
	approvalAsked  = regexp.MustCompile(`(^|\s)--asked\b`)
	namedFormat    = regexp.MustCompile(`["']?format["']?\s*:\s*["']?(markdown|shale)\b`)
)

// approvalCommand reports whether command records an approval, and of which
// change ("" for `stele approve`).
func approvalCommand(command string) (change string, ok bool) {
	if m := approvalAnswer.FindStringSubmatch(command); m != nil {
		return strings.Trim(m[1], `"'`), true
	}
	if m := approvalDirect.FindStringSubmatch(command); m != nil && !approvalList.MatchString(m[1]) {
		return "", true
	}
	return "", false
}

// approval asks the owner before an approval is recorded. Where Claude
// Code cannot ask (permissions bypassed or never asked, or a headless run
// that holds every prompt), the agent asks in chat and says so with
// --asked. A headless run held the command, the agent asked in chat, and
// the same command with --asked was held again (a trial, 2026-11-09): the
// flag passes in every mode. The prompt appears even where an allow rule
// covers stele's commands. STELE_APPROVAL_PROMPT=off, set where Claude Code
// starts, leaves approvals to unattended runs.
func (h *hook) approval(project string, input hookInput) {
	if input.ToolName != "Bash" || os.Getenv("STELE_APPROVAL_PROMPT") == "off" {
		return
	}
	command, _ := input.ToolInput.Command.(string)
	change, ok := approvalCommand(command)
	if !ok || approvalAsked.MatchString(command) {
		return
	}
	decide := func(decision, reason string) {
		h.say("PreToolUse", map[string]any{"permissionDecision": decision, "permissionDecisionReason": reason})
	}
	if input.PermissionMode == "bypassPermissions" || input.PermissionMode == "dontAsk" {
		decide("deny", "Stele: the owner approves a change, and Claude Code is not asking for permissions in this session, so the owner has not seen this approval. "+
			"Show the owner the spec and the check's summary and ask them the approval question in chat. When they say yes, run the same command again with --asked added. Never add it before they answer.")
		return
	}
	what := ""
	if change != "" {
		if r := steleRun(project, 4*time.Second, "", "change", "summary", change); r.err == nil {
			what = strings.TrimSpace(r.stdout)
		}
	}
	// A change in a requirements/ folder or of kept requests is named after
	// its branch, which tells the owner nothing: leave the name out there.
	config, _ := os.ReadFile(filepath.Join(project, "stele.yaml"))
	reason := "Stele: this records your approval"
	if change != "" && !namedFormat.Match(config) {
		reason += fmt.Sprintf(" of %q", change)
	}
	reason += " as the owner of this project's requirements."
	if what != "" {
		reason += " " + what
	}
	// The owner reads this in the prompt. A headless run hands it to the
	// agent instead, whose instructions say to ask in chat (--asked).
	decide("ask", reason+" Allow it to approve; deny it to hold the change back.")
}

// hasSpecs: `specs:` at the start of a line, or `"specs":` in the flow
// (JSON) form YAML also reads. A project whose stele.yaml names no specs
// has none to keep in sync: say nothing, rather than a failure on every
// command (2026-11-07).
var hasSpecs = regexp.MustCompile(`(?m)(^|[{,]\s*)["']?specs["']?\s*:`)

var hookNext = regexp.MustCompile("`([^`]+)`")

// In the project's words and paths: the note named absolute paths,
// diagnostic codes and "stable rule IDs" (a review, 2026-11-09).
var plainDiagnostic = map[string]string{
	"spec.added":      "a new requirement, not checked yet",
	"spec.changed":    "edited since the owner approved it",
	"spec.moved":      "moved; nothing to decide",
	"spec.provenance": "moved in its file: run `stele sync --apply` to record where it is now; nothing to decide",
	"spec.removed":    "approved, and no longer in the specs",
	"rule.stale":      "approved for text that has since changed",
	"rule.orphaned":   "approved, and its requirement is gone",
}

type watchStatus struct {
	InSync      bool             `json:"in_sync"`
	Next        string           `json:"next"`
	Pending     []map[string]any `json:"pending"`
	StaleRules  []any            `json:"stale_rules"`
	OrphanRules []any            `json:"orphan_rules"`
	Diagnostics []struct {
		Code      string `json:"code"`
		Statement string `json:"statement"`
		File      string `json:"file"`
		Line      any    `json:"line"`
		Message   string `json:"message"`
	} `json:"diagnostics"`
}

// afterTool: saved-spec drift, requests waiting on the owner, and the
// approved requirements the code change touches, each said once.
func (h *hook) afterTool(project string, input hookInput) error {
	var notes []string
	sum := sha256.Sum256([]byte(project + "\x00" + input.Session))
	key := hex.EncodeToString(sum[:])
	cacheDir := os.Getenv("STELE_HOOK_CACHE")
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	// Say a thing once: each part is repeated only when it changes.
	changed := func(part, text string) (now bool, before string) {
		file := filepath.Join(cacheDir, "stele-hook-"+key+part)
		d := sha256.Sum256([]byte(text))
		digest := hex.EncodeToString(d[:])
		if b, err := os.ReadFile(file); err == nil {
			before = string(b)
		}
		os.WriteFile(file, []byte(digest), 0o600)
		return before != digest, before
	}
	config, _ := os.ReadFile(filepath.Join(project, "stele.yaml"))
	if hasSpecs.Match(config) {
		r := steleRun(project, 10*time.Second, "", "watch", "--project", project, "--once", "--json")
		if r.err != nil {
			why := strings.TrimSpace(r.why())
			if now, _ := changed("-error", why); now {
				notes = append(notes, fmt.Sprintf("Stele could not compare the specs with what the owner approved: %s. Do not assume they match.", why))
			}
		} else {
			var status watchStatus
			if err := json.Unmarshal([]byte(r.stdout), &status); err != nil {
				return err
			}
			notes = append(notes, waitingNote(status, input.Session, changed)...)
			if now, before := changed("", r.stdout); now && status.InSync {
				if before != "" {
					notes = append(notes, "Stele: the specs match what the owner approved again.")
				}
			} else if now {
				notes = append(notes, driftNote(project, status))
			}
		}
	}
	// The approved requirements the code change touches, when that set changes.
	if r := steleRun(project, 8*time.Second, "", "touches", "--project", project); r.err == nil {
		text := strings.TrimSpace(r.stdout)
		var ids []string
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(l, "- ") {
				ids = append(ids, l)
			}
		}
		if now, _ := changed("-touches", strings.Join(ids, "\n")); now && text != "" {
			notes = append(notes, text)
		}
	}
	if len(notes) > 0 {
		h.say("PostToolUse", map[string]any{"additionalContext": strings.Join(notes, "\n\n")})
	}
	return nil
}

// waitingNote: a request declared with shale intent that the owner has not
// approved; the code written for it waits on that approval. Said once per
// set of waiting requests; an agent once built a feature and asked after.
// This session's requests by name (a request's ID carries the session it
// was declared in), the others as a count: 25 waiting requests were all
// listed each time one was added (2026-11-06).
func waitingNote(status watchStatus, session string, changed func(string, string) (bool, string)) []string {
	type request struct{ id, prose string }
	var waiting []request
	for _, s := range status.Pending {
		id := fmt.Sprint(firstOf(s, "ID", "id"))
		if strings.HasPrefix(id, "intent/") {
			prose := ""
			if p := firstOf(s, "Prose", "prose"); p != nil {
				prose = fmt.Sprint(p)
			}
			waiting = append(waiting, request{id, prose})
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	ids := make([]string, len(waiting))
	for i, s := range waiting {
		ids[i] = s.id
	}
	if now, _ := changed("-requests", strings.Join(ids, "\n")); !now {
		return nil
	}
	var named []request
	if session != "" {
		for _, s := range waiting {
			if strings.Contains(s.id, session) {
				named = append(named, s)
			}
		}
	}
	if len(named) == 0 {
		named = waiting
	}
	named = named[max(0, len(named)-3):]
	others := len(waiting) - len(named)
	titles := make([]string, len(named))
	for i, s := range named {
		first, _, _ := strings.Cut(s.prose, "\n")
		if r := []rune(first); len(r) > 120 {
			first = string(r[:120])
		}
		titles[i] = `"` + first + `"`
	}
	subject, pronoun := "these requests are", "them"
	if len(named) == 1 {
		subject = "this request is"
		if others == 0 {
			pronoun = "it"
		}
	}
	more := ""
	if others > 0 {
		more = fmt.Sprintf(" (and %d other%s)", others, plural(others))
	}
	run := "Stele's change check"
	if m := hookNext.FindStringSubmatch(status.Next); m != nil {
		run = "`" + m[1] + "`"
	}
	return []string{fmt.Sprintf("Stele: %s not approved by the owner yet: %s%s. Before more edits for %s, run %s and put its questions, and the approval, to the owner.",
		subject, strings.Join(titles, "; "), more, pronoun, run)}
}

func firstOf(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

// driftNote: the specs differ from what the owner approved, one line a
// requirement (its edit says what the rule's line would), in the
// project's paths.
func driftNote(project string, status watchStatus) string {
	where := func(file string) string {
		rel, err := filepath.Rel(project, filepath.Join(project, file))
		if filepath.IsAbs(file) {
			rel, err = filepath.Rel(project, file)
		}
		if err != nil || strings.HasPrefix(rel, "..") {
			return filepath.Base(file)
		}
		return filepath.ToSlash(rel)
	}
	var counts []string
	count := func(n int, what string) {
		if n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, what))
		}
	}
	count(len(status.Pending), "new or edited")
	count(len(status.StaleRules), "approved for text that has since changed")
	count(len(status.OrphanRules), "approved and no longer in the specs")
	edited := map[string]bool{}
	for _, d := range status.Diagnostics {
		if d.Code == "spec.changed" {
			edited[d.Statement] = true
		}
	}
	var lines []string
	for _, d := range status.Diagnostics {
		if d.Code == "rule.stale" && edited[d.Statement] {
			continue
		}
		what := plainDiagnostic[d.Code]
		if what == "" {
			what = d.Message
		}
		subject := d.Statement
		if subject == "" {
			subject = where(d.File)
		}
		line := "- " + subject + ": " + what
		if strings.HasPrefix(d.Code, "spec.") && d.Code != "spec.removed" {
			line += fmt.Sprintf(" (%s:%v)", where(d.File), jsonNumber(d.Line))
		}
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	text := "Stele: the specs differ from what the owner approved"
	if len(counts) > 0 {
		text += " (requirements: " + strings.Join(counts, ", ") + ")"
	}
	text += ".\n" + strings.Join(lines[:min(8, len(lines))], "\n")
	if len(lines) > 8 {
		text += fmt.Sprintf("\n(and %d more)", len(lines)-8)
	}
	if status.Next != "" {
		text += "\nNext: " + status.Next + ". Nothing is checked or approved until then."
	}
	return text
}

// jsonNumber prints a decoded JSON number as written: 3, not 3e+00.
func jsonNumber(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

type runResult struct {
	stdout, stderr string
	err            error
	timedOut       bool
}

// why is the failure as the agent should read it.
func (r runResult) why() string {
	switch {
	case r.timedOut:
		return "stele did not answer in time"
	case r.stderr != "":
		return r.stderr
	case r.stdout != "":
		return r.stdout
	}
	return r.err.Error()
}

// steleRun runs Stele itself (STELE_BIN when set) in the project, with a
// time limit inside the agent's own hook timeout.
func steleRun(project string, limit time.Duration, stdin string, args ...string) runResult {
	bin := os.Getenv("STELE_BIN")
	if bin == "" {
		if self, err := os.Executable(); err == nil {
			bin = self
		} else {
			bin = "stele"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = project
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := runResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		r.timedOut, r.err = true, ctx.Err()
	}
	return r
}
