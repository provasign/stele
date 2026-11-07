// Package integrations embeds what `stele init` installs into a project for
// the coding agents: the agent skill, the short steering text every agent's
// instruction file gets, and the Claude Code and Codex hooks. The files here are the
// only copies; `stele init` writes them out.
package integrations

import _ "embed"

// Skill is the full stele-change procedure, for agents with skills
// (Claude Code, Codex CLI).
//
//go:embed skills/stele-change/SKILL.md
var Skill string

// Steering is the short loop written into each agent's instruction file.
//
//go:embed skills/stele-change/steering.md
var Steering string

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
