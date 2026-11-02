# Agent skills

`stele-change/SKILL.md` lets a coding agent check an OpenSpec change with Stele and put the decisions it needs to the owner as questions (D-058). It follows the Agent Skills format, so one file serves both agents.

Set it up with `stele init`, which installs Stele's loop for every coding agent Prism supports:

```sh
stele init --harness claude,codex,cursor,windsurf,vscode,gemini,opencode .   # a new or an existing project
stele init --yes .                                                            # refresh the agents recorded in stele.yaml
```

| Agent | What `stele init` writes |
| --- | --- |
| Claude Code (`claude`) | a Stele section in `CLAUDE.md`, this skill in `.claude/skills/stele-change/`; `--hook` adds the PostToolUse hook in `.claude/hooks/` and `.claude/settings.json` |
| Codex CLI (`codex`) | a Stele section in `AGENTS.md`, this skill in `.agents/skills/stele-change/` |
| Cursor, Windsurf, opencode | a Stele section in `AGENTS.md` |
| VS Code / GitHub Copilot (`vscode`) | a Stele section in `.github/copilot-instructions.md` |
| Gemini CLI (`gemini`) | a Stele section in `GEMINI.md` |

The section (`steering.md`) is the loop in short; it sits between `<!-- stele:begin -->` and `<!-- stele:end -->` markers, so the rest of each file is never touched, a shared or symlinked file gets one section, and running `init` again changes nothing. Deselecting an agent removes its section and its unedited skill copy. Commit the files so every teammate's agent runs the same loop. Interactive `stele init` recommends the agents it finds installed or already used in the project.

The agent then runs `stele change openspec . <id> --json` after a change is written, answers Stele's own misreadings itself, asks the owner the rest in the spec's words (Claude Code's question picker, or numbered options in Codex), records each answer with `stele change answer`, and finishes with the approval, which writes the change's rules into the registry. `stele gate` stays the backstop where a project runs it in CI (a GitHub integration is planned).
