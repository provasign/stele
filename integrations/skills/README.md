# Agent skills

`stele-change/SKILL.md` lets a coding agent check a spec change with Stele and put the decisions it needs to the owner as questions (D-058). It follows the Agent Skills format.

Stele supports Claude Code for now. `stele init` sets it up:

```sh
stele init .         # a new or an existing project
stele init --yes .   # refresh what stele.yaml records
```

| What `stele init` writes | Why |
| --- | --- |
| A Stele section in `CLAUDE.md` (`steering.md` and the part for the project's spec format) | The loop in short: when to run the check, how to put its questions to the owner, and only what the project's own format needs (OpenSpec, Spec Kit, Kiro, a `requirements/` folder, or Shale requests) |
| This skill in `.claude/skills/stele-change/` | The loop in full, for when the agent edits specs or a Stele message says so |
| `.claude/hooks/stele-hook.js` and its entries in `.claude/settings.json` (skip with `--hook=false`) | Gives the agent the approved requirements a request touches, tells it when saved specs drift, and has Claude Code ask the owner before an approval is recorded |

The section sits between `<!-- stele:begin -->` and `<!-- stele:end -->` markers, so the rest of `CLAUDE.md` is never touched, a symlinked file gets one section, and running `init` again changes nothing. Commit the files so every teammate's agent runs the same loop.

The agent runs the check as soon as a spec draft is written, shows the owner the spec with the check's summary under it, answers Stele's own misreadings itself, and asks the owner the rest in the spec's words. It records each answer with `stele change answer` and finishes with the approval, which writes the change's rules into the registry.

**The approval prompt.** The hook's `PreToolUse` entry watches for the two commands that record an approval (`stele change answer <change> <question> approve` and `stele approve`). It answers Claude Code with "ask", so the owner sees a permission prompt with the check's summary even where an allow rule covers the command. In a session with permissions bypassed, where Claude Code would not ask, the hook refuses the command and tells the agent to ask in chat and add `--asked` once the owner says yes. A headless run (`claude -p`) holds the prompt with no one to answer it, and the agent does the same there. `STELE_APPROVAL_PROMPT=off`, set where Claude Code starts, turns the prompt off for unattended runs. The prompt holds back an eager agent; it is not a security boundary, since an agent that means to get round it can. `stele gate` stays the backstop where a project runs it in CI.

Claude Code is the one agent `stele init` sets up. Setups for other agents are in the code and not offered yet.
