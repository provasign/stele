package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/provasign/stele/integrations"
	"github.com/provasign/stele/internal/config"
)

// harness is a coding agent Stele's loop is installed into: its
// instruction file, and where its skills live when it has them. The set
// and the files follow Prism's `prism init --harness` (D-062 keeps Stele
// free of Prism, so this is Stele's own copy of the table).
type harness struct {
	id, label    string
	instructions string   // the file the agent reads its project instructions from
	skills       string   // skills directory, when the agent has skills
	commands     []string // executables whose presence suggests the agent is installed
	markers      []string // project paths that show the agent is used here
	supported    bool     // offered by init; the others stay for projects that recorded them
}

// Stele supports Claude Code only for now (the owner's decision,
// 2026-11-09). init detects and offers it alone and refuses the others by
// name; a project whose stele.yaml already records another keeps it, so an
// existing setup still refreshes.
var harnesses = []harness{
	{id: "claude", label: "Claude Code", instructions: "CLAUDE.md", skills: ".claude/skills", commands: []string{"claude"}, markers: []string{".claude", "CLAUDE.md"}, supported: true},
	{id: "codex", label: "Codex CLI", instructions: "AGENTS.md", skills: ".agents/skills", commands: []string{"codex"}, markers: []string{".codex", ".agents"}},
	{id: "cursor", label: "Cursor", instructions: "AGENTS.md", commands: []string{"cursor", "cursor-agent"}, markers: []string{".cursor"}},
	{id: "windsurf", label: "Windsurf", instructions: "AGENTS.md", commands: []string{"windsurf"}, markers: []string{".windsurf", ".windsurfrules"}},
	{id: "vscode", label: "VS Code (GitHub Copilot)", instructions: ".github/copilot-instructions.md", commands: []string{"code"}, markers: []string{".vscode", ".github/copilot-instructions.md"}},
	{id: "gemini", label: "Gemini CLI", instructions: "GEMINI.md", commands: []string{"gemini"}, markers: []string{".gemini", "GEMINI.md"}},
	{id: "opencode", label: "opencode", instructions: "AGENTS.md", commands: []string{"opencode"}, markers: []string{"opencode.json", ".opencode"}},
}

const (
	steeringBegin = "<!-- stele:begin (written by `stele init`; edits inside are replaced) -->"
	steeringEnd   = "<!-- stele:end -->"
	skillName     = "stele-change"
)

func harnessIDs() []string {
	var ids []string
	for _, h := range harnesses {
		if h.supported {
			ids = append(ids, h.id)
		}
	}
	return ids
}

// parseHarnesses checks a comma-separated harness list.
func parseHarnesses(list string) ([]string, error) {
	var out []string
	for _, id := range strings.Split(list, ",") {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if id == "claude-code" {
			id = "claude"
		}
		i := slices.IndexFunc(harnesses, func(h harness) bool { return h.id == id })
		if i < 0 {
			return nil, fmt.Errorf("unknown agent %q; known: %s", id, strings.Join(harnessIDs(), ", "))
		}
		if !harnesses[i].supported {
			return nil, fmt.Errorf("Stele supports Claude Code only for now; %s is not supported in this preview", harnesses[i].label)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// detectHarnesses lists the agents installed on this machine or already
// used in the project, for init to recommend.
func detectHarnesses(dir string) []string {
	var out []string
	for _, h := range harnesses {
		if !h.supported {
			continue
		}
		found := false
		for _, c := range h.commands {
			if _, err := exec.LookPath(c); err == nil {
				found = true
			}
		}
		for _, m := range h.markers {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				found = true
			}
		}
		if found {
			out = append(out, h.id)
		}
	}
	return out
}

// installSteering writes Stele's loop into the instruction file of every
// selected agent and the full skill where the agent has skills, and removes
// Stele's section from instruction files no selected agent reads any more.
// It returns the files it changed. Several agents can share one file
// (AGENTS.md), and one file can be another's symlink (CLAUDE.md ->
// AGENTS.md): each real file gets one section.
func installSteering(dir string, selected []string, hook bool, specs *config.Specs) ([]string, error) {
	// Only what the project's way of keeping specs needs.
	format, root := "", ""
	if specs != nil {
		format, root = specs.Format, filepath.ToSlash(specs.Root)
	}
	text := integrations.SteeringFor(format, root)
	want := map[string]bool{}     // real instruction file -> wanted
	byReal := map[string]string{} // real path -> path as named
	var order []string
	for _, h := range harnesses {
		path := filepath.Join(dir, h.instructions)
		real := realPath(path)
		if _, seen := byReal[real]; !seen {
			byReal[real] = path
			order = append(order, real)
		}
		if slices.Contains(selected, h.id) {
			want[real] = true
		}
	}
	var changed []string
	for _, real := range order {
		ok, err := setSection(byReal[real], want[real], text)
		if err != nil {
			return changed, err
		}
		if ok {
			changed = append(changed, byReal[real])
		}
	}
	for _, h := range harnesses {
		if h.skills == "" {
			continue
		}
		path := filepath.Join(dir, h.skills, skillName, "SKILL.md")
		if !slices.Contains(selected, h.id) {
			// A deselected agent's copy goes, unless someone edited it.
			if old, err := os.ReadFile(path); err == nil && string(old) == integrations.Skill {
				if err := os.Remove(path); err != nil {
					return changed, err
				}
				_ = os.Remove(filepath.Dir(path))
				changed = append(changed, path)
			}
			continue
		}
		ok, err := writeIfChanged(path, integrations.Skill)
		if err != nil {
			return changed, err
		}
		if ok {
			changed = append(changed, path)
		}
	}
	if slices.Contains(selected, "codex") {
		// Without it, every check under Codex fails in the sandbox.
		path := filepath.Join(dir, ".codex", "rules", "stele.rules")
		ok, err := writeIfChanged(path, integrations.CodexRules)
		if err != nil {
			return changed, err
		}
		if ok {
			changed = append(changed, path)
		}
	}
	for _, h := range agentHooks {
		if !hook || !slices.Contains(selected, h.agent) {
			continue
		}
		files, err := installHook(dir, h)
		changed = append(changed, files...)
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// agentHook is where an agent with hooks gets the hook script, and the
// file its hook configuration is merged into.
type agentHook struct {
	agent, script, config, entries string
}

var agentHooks = []agentHook{
	{agent: "claude", script: ".claude/hooks/stele-hook.js", config: ".claude/settings.json", entries: integrations.HookSettings},
	{agent: "codex", script: ".codex/hooks/stele-hook.js", config: ".codex/hooks.json", entries: integrations.CodexHooks},
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// setSection puts Stele's marked section into path, or takes it out, and
// leaves the rest of the file as it is. It reports whether the file changed.
func setSection(path string, want bool, steering string) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	text := string(src)
	section := steeringBegin + "\n" + strings.TrimSpace(steering) + "\n" + steeringEnd + "\n"
	var next string
	i, j := strings.Index(text, steeringBegin), strings.Index(text, steeringEnd)
	switch {
	case i >= 0 && j > i:
		rest := text[j+len(steeringEnd):]
		rest = strings.TrimPrefix(rest, "\n")
		if want {
			next = text[:i] + section + rest
		} else {
			next = strings.TrimRight(text[:i], "\n") + "\n" + rest
			if strings.TrimSpace(next) == "" {
				next = ""
			}
		}
	case want:
		next = text
		if strings.TrimSpace(next) != "" {
			next = strings.TrimRight(next, "\n") + "\n\n"
		}
		next += section
	default:
		return false, nil // not wanted and not there
	}
	if next == text {
		return false, nil
	}
	if next == "" && len(src) > 0 {
		return true, os.Remove(path) // the file held only Stele's section
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(next), 0o644)
}

func writeIfChanged(path, content string) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(content), 0o644)
}

// installHook writes an agent's hook script and adds each of its events
// (UserPromptSubmit: the requirements a request touches; PostToolUse:
// saved-spec drift) to the agent's hook configuration once, keeping every
// other setting and hook.
func installHook(dir string, h agentHook) ([]string, error) {
	var changed []string
	script := filepath.Join(dir, h.script)
	if ok, err := writeIfChanged(script, integrations.Hook); err != nil {
		return changed, err
	} else if ok {
		changed = append(changed, script)
	}
	path := filepath.Join(dir, h.config)
	settings := map[string]any{}
	if src, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(src, &settings); err != nil {
			return changed, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return changed, err
	}
	var ours map[string]any
	if err := json.Unmarshal([]byte(h.entries), &ours); err != nil {
		return changed, err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	added := false
	ourHooks := ours["hooks"].(map[string]any)
	for _, event := range slices.Sorted(maps.Keys(ourHooks)) {
		entries, _ := hooks[event].([]any)
		installed := false
		for _, e := range entries {
			if js, _ := json.Marshal(e); strings.Contains(string(js), "stele-hook.js") {
				installed = true
			}
		}
		if !installed {
			hooks[event] = append(entries, ourHooks[event].([]any)[0])
			added = true
		}
	}
	if !added {
		return changed, nil // already installed
	}
	settings["hooks"] = hooks
	// Commands as written, for the user who reviews them (Codex's /hooks).
	var js bytes.Buffer
	enc := json.NewEncoder(&js)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return changed, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return changed, err
	}
	return append(changed, path), os.WriteFile(path, js.Bytes(), 0o644)
}

func harnessLabels(ids []string) string {
	var out []string
	for _, h := range harnesses {
		if slices.Contains(ids, h.id) {
			out = append(out, h.label)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
