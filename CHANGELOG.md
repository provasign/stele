# Changelog

## Unreleased

- **Shale requests as specs.** `stele change shale . --base main` checks the requests a branch's agents declared with `shale intent` against every approved one, including a session still open, before the agent edits. `stele init` picks this format in a repository with `.shale/` and no spec folder. A conflict with an earlier request offers to retire it (`.stele/retired-intents.yaml`) or to restate both as one request.
- **Code touches.** `stele touches` lists the approved requirements the branch's code touches, from Grove's parse of the changed files: a function named like a rule's step, or code that is, reads or writes an attribute the rule reads. The post-edit hook says so when that set changes.
- **PR card.** `stele change`, the owner's answers and the pre-commit hook keep `.stele/cards/<change>.json` current: status, requirements added, changed and retired, the owner's decisions, and the approved requirements the code touches. Shale renders it on the PR.
- The watch hook's message names the command that checks pending statements.
- The pre-commit hook no longer asks to run `stele adopt` when there is nothing to adopt, such as a Shale project before its first request.

## v0.1.0

The first public release. Stele is experimental; [what to expect](README.md#what-to-expect) states measured accuracy and cost.

- **Setup.** `stele init` detects the spec format (Spec Kit, OpenSpec, Kiro or plain Markdown) and the coding agents in the repository, and writes Stele's instructions, a skill where the agent has skills, the agent hooks and a git pre-commit hook. Agents: Claude Code, Codex, Cursor, Windsurf, VS Code (Copilot), Gemini CLI and opencode.
- **Adoption.** `stele adopt` translates every requirement once into rules over one vocabulary, checks the rules against each other with Z3, and asks the owner about each contradiction in the spec's own words.
- **Changes.** `stele change openspec|speckit|kiro` checks one spec change against every approved requirement, translating only what the change adds or edits. `stele change answer` records the owner's answers; approvals are pinned to the requirement's text.
- **Prevention.** Before the agent plans, the prompt hook adds the approved requirements a request touches (`stele rules`). After an edit, the hook reports saved specs that no longer match their approved rules. Neither hook calls a model.
- **Gate.** `stele gate` in CI and `stele precommit` locally block unapproved or conflicting rules.
- **Rules.** Permissions, preconditions and invariants over states and steps, with step results, multiple instances, durations and `min`, `max`, `abs`. Each conflict comes with a minimal set of requirements and a counterexample.
- **Cost control.** `--max-cost` caps model spend for `stele adopt` and `stele change`; a stopped run keeps the work it finished.
- **Install.** `install.sh` and Homebrew (`provasign/tap/stele`), for Linux and macOS on amd64 and arm64. Z3 is required on PATH.
