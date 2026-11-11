# Changelog

## v0.1.19 - 2026-11-11

The first launch. Stele works with Claude Code, on macOS and Linux.

- **Reads the specs a project already writes**: Spec Kit, OpenSpec, Kiro and plain Markdown (`MUST`, `SHALL`, `SHOULD`, `MAY`); a project with none gets a `requirements/` folder its agent keeps.
- **Checks every requirement against every other.** Each becomes a rule over one vocabulary shared by the project; a solver finds rules that cannot all hold and shows a situation; a review of the texts lists contradictions the rules do not prove.
- **Asks the owner only what needs a person,** in the specs' own words, with what the code does today. Existing specs are adopted whole or a few at a time, and a contradiction can be decided later.
- **Shows what is approved.** Each reading is shown clause by clause in plain words, with what checks it or that nothing does. An approval is pinned to the text, the reading and the vocabulary it accepted.
- **Guards what was approved.** The agent is handed the approved requirements each request touches; a pre-commit hook and `stele gate` block a change that breaks one, or that cannot be checked.
- **Keeps little.** The registry, approvals and lessons live in the repository. No prompt or reply is stored beyond a failed reading's, until its change is approved. Project text reaches the model as material, never as instructions. `--max-cost` limits a whole check.

[What to expect](README.md#what-to-expect) has the measurements. Preview builds before this one are listed under the repository's releases.
