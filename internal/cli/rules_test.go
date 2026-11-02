package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rulesRegistry = `
context: orders
formatVersion: 1
---
entity: Order
context: orders
attributes:
  shipped: { type: bool, controller: system }
  refunded: { type: bool, controller: system }
---
operation: Order.cancel
context: orders
default: allow
writes: [Order.shipped]
---
id: O1
kind: precondition
operation: Order.cancel
when: not self.shipped
source: { statementId: "orders.md#O1", doc: "orders.md#L3", prose: "An order MUST NOT be cancelled once it has shipped." }
---
id: O2
kind: invariant
assert: not (self.shipped and self.refunded)
subject: Order
source: { statementId: "orders.md#O2", doc: "orders.md#L5", prose: "A shipped order MUST NOT be refunded automatically; refunds of shipped orders go through returns." }
`

// TestRules: a request gets the approved requirements it touches, in the
// spec's words, and a hint for each operation it names; a request about
// nothing the specs say gets nothing; --op lists an operation's rules.
func TestRules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(rulesRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		var out bytes.Buffer
		if code := cmdRules(append([]string{"--registry", dir}, args...), strings.NewReader(""), &out); code != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
		return out.String()
	}
	got := run("Let customers cancel an order after it has shipped")
	for _, want := range []string{"- An order MUST NOT be cancelled once it has shipped. (orders.md#L3)", "ask the owner"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if got := run("Bump all dependencies to their latest versions"); got != "" {
		t.Errorf("an unrelated request got:\n%s", got)
	}
	var js struct {
		Statements []constraint `json:"statements"`
	}
	if err := json.Unmarshal([]byte(run("--op", "Order.cancel", "--json")), &js); err != nil || len(js.Statements) != 1 || js.Statements[0].Rules[0] != "O1" {
		t.Errorf("--op: %+v %v", js, err)
	}
}

// TestHookUpgrade: a project set up with only the post-edit hook gains the
// prompt hook, and nothing is added twice.
func TestHookUpgrade(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	old := `{"model": "x", "hooks": {"PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "node \"$CLAUDE_PROJECT_DIR/.claude/hooks/stele-hook.js\""}]}]}}`
	os.WriteFile(settings, []byte(old), 0o644)
	for range 2 {
		if _, err := installHook(dir, agentHooks[0]); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(settings)
	var s struct {
		Model string                      `json:"model"`
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Model != "x" || len(s.Hooks["PostToolUse"]) != 1 || len(s.Hooks["UserPromptSubmit"]) != 1 {
		t.Errorf("settings after upgrade: %s", b)
	}
}

// TestRulesSaysWhatIsApproved: "approved" only when the owner approved every
// requirement shown; otherwise the ones not yet approved are marked.
func TestRulesSaysWhatIsApproved(t *testing.T) {
	approve := func(reg, id string) string {
		return strings.Replace(reg, `source: { statementId: "orders.md#`+id+`",`, `source: { textHash: "h`+id+`", approvedBy: owner, approvedHash: "h`+id+`", statementId: "orders.md#`+id+`",`, 1)
	}
	run := func(reg string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(reg), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := cmdRules([]string{"--registry", dir, "Let customers cancel an order after it has shipped"}, strings.NewReader(""), &out); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return out.String()
	}
	mixed := run(approve(rulesRegistry, "O1"))
	if strings.Contains(mixed, "approved requirements") ||
		!strings.Contains(mixed, "has shipped. (orders.md#L3)\n") ||
		!strings.Contains(mixed, "go through returns. (orders.md#L5) [not yet approved by the owner]") {
		t.Errorf("one approved, one not:\n%s", mixed)
	}
	all := run(approve(approve(rulesRegistry, "O1"), "O2"))
	if !strings.Contains(all, "approved requirements") || strings.Contains(all, "not yet approved") {
		t.Errorf("all approved:\n%s", all)
	}
}
