// Package integrations embeds what `stele init` installs into a project for
// the coding agents: the agent skill, the short steering text every agent's
// instruction file gets, and the Claude Code and Codex hooks. The files here are the
// only copies; `stele init` writes them out.
package integrations

import (
	_ "embed"
	"strings"
)

// Skill is the full stele-change procedure, for agents with skills
// (Claude Code, Codex CLI).
//
//go:embed skills/stele-change/SKILL.md
var Skill string

// The short loop written into each agent's instruction file: the steps
// every project follows, and a part for each way a project keeps its specs.
//
//go:embed skills/stele-change/steering.md
var steering string

//go:embed skills/stele-change/steering-shale.md
var steeringShale string

//go:embed skills/stele-change/steering-markdown.md
var steeringMarkdown string

//go:embed skills/stele-change/steering-adopt.md
var steeringAdopt string

const shaleNote = " Where Shale is installed, declare the task first with `shale intent \"<title>\" --for <change-id>`, naming the spec change it implements."

// SteeringFor is the loop for a project whose specs are in format
// (stele.yaml's specs.format) under root: only what that format needs. An
// agent given every format's instructions read 1,100 words before its
// first request, most of them about tools the project does not use (a
// review, 2026-11-09). A format Stele does not know gets them all.
func SteeringFor(format, root string) string {
	when, check, shale, part := "", "", shaleNote, steeringAdopt
	switch format {
	case "openspec":
		when, check = "Whenever you write or edit specs (OpenSpec `openspec/changes/`)", "`stele change openspec . <change-id> --json`"
	case "speckit":
		when, check = "Whenever you write or edit specs (Spec Kit `specs/`)", "`stele change speckit . --base <branch> --json`"
	case "kiro":
		when, check = "Whenever you write or edit specs (Kiro `.kiro/specs/`)", "`stele change kiro . --base <branch> --json`"
	case "shale":
		when, check = "Whenever you keep a request as a requirement (below)", "`stele change shale . --base <branch> --json`"
		shale, part = "", steeringShale
	case "markdown":
		when, check = "Whenever you write or edit a requirement (Markdown under `"+root+"/`, below)", "`stele change markdown . --json`"
		shale, part = "", strings.ReplaceAll(steeringMarkdown, "{{root}}", root)
	default:
		when = "Whenever you write or edit specs (OpenSpec `openspec/changes/`, Spec Kit `specs/`, Kiro `.kiro/specs/`)"
		check = "`stele change openspec . <change-id> --json` (OpenSpec), or `stele change speckit|kiro . --base <branch> --json` (Spec Kit, Kiro),"
		part = steeringShale + "\n" + steeringAdopt
	}
	return strings.NewReplacer("{{when}}", when, "{{check}}", check, "{{shale}}", shale, "{{format}}", strings.TrimSpace(part)).Replace(steering)
}

// Hook is the hook script for Claude Code and Codex, whose hook events
// share one input and output format, and HookSettings its Claude Code
// settings entry.
//
//go:embed claude/stele-hook.js
var Hook string

//go:embed claude/settings.example.json
var HookSettings string

// CodexHooks is the hook script's entry in a project's .codex/hooks.json.
// Codex sets no project-directory variable for hooks, so the command finds
// the script from the repository root; it runs after every tool, since
// the drift check takes milliseconds.
//
//go:embed codex/hooks.example.json
var CodexHooks string

// CodexRules is the project rule that lets `stele change` and `stele
// adopt` run outside Codex's sandbox, in .codex/rules/stele.rules: they call
// the model through the codex CLI, which cannot start inside it.
//
//go:embed codex/stele.rules
var CodexRules string
