# Agent skills

`stele-change/SKILL.md` lets a coding agent check a spec change with Stele and put the decisions it needs to the owner as questions (D-058). It follows the Agent Skills format.

Stele supports Claude Code for now. `stele init` sets it up:

```sh
stele init .         # a new or an existing project
stele init --yes .   # refresh what stele.yaml records
```

| What `stele init` writes | Why |
| --- | --- |
| A Stele section in `CLAUDE.md` (`steering.md`) | The loop in short: when to run the check, how to put its questions to the owner, when to keep a Shale request |
| This skill in `.claude/skills/stele-change/` | The loop in full, for when the agent edits specs or a Stele message says so |
| `.claude/hooks/stele-hook.js` and its entries in `.claude/settings.json` (skip with `--hook=false`) | Gives the agent the approved requirements a request touches, and tells it when saved specs drift |

The section sits between `<!-- stele:begin -->` and `<!-- stele:end -->` markers, so the rest of `CLAUDE.md` is never touched, a symlinked file gets one section, and running `init` again changes nothing. Commit the files so every teammate's agent runs the same loop.

The agent runs the check as soon as a spec draft is written, shows the owner the spec with the check's summary under it, answers Stele's own misreadings itself, and asks the owner the rest in the spec's words. It records each answer with `stele change answer` and finishes with the approval, which writes the change's rules into the registry. `stele gate` stays the backstop where a project runs it in CI.

The setups for Codex, Cursor, Windsurf, VS Code (Copilot), Gemini CLI and opencode remain in the code for projects whose `stele.yaml` already records them, but `stele init` no longer offers them.
