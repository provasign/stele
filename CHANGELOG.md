# Changelog

## v0.1.5 - 2026-11-03

Found by three developer trials (ten features each, built with Claude Code and Stele from the owner's requests) and a code review.

- **The owner's decisions take fewer turns.**
  - **Precedence:** a new request that adds a case to an approved one can take precedence where they meet, both standing; no restating of the whole rule.
  - **Restated requests:** a request restated after the owner retired the earlier wording is the same requirement, edited, with no rename question.
  - **Changed numbers:** a request whose rule is an approved rule with only its numbers changed ("at most 2" for "at most 3") asks whether it replaces it, however it is worded.
- **More of what a request says is checked, and the rest is named.**
  - **Exact computed values decide:** "the whole days overdue", stated by two bounds, and "the message names the overdue tool" are decided where the solver used to give up.
  - **Results per item decide:** a uniqueness rule makes its attribute a key, so the tools a step reads with the same id are one tool, and results keyed by id have one value each.
  - **Other instances:** state invariants hold for the other instances a rule ranges over.
  - **Partial checks are named:** a reading Stele could not use is a finding of its requirement, so a requirement checked in part says which part is not.
  - **Free settings are asked about:** a threshold read through a new setting no requirement gives a value is a question: the owner says whether a fixed value was meant.
  - **Undecided steps:** the owner can approve a step the solver cannot decide; the registry records it as unchecked and CI's gate accepts only recorded steps.
- **Fewer false contradictions.**
  - **Unreachable states:** a clash in a state no step reaches is set aside more often. Reachability now fixes whether optional values are set and leaves out entities with no creating step. When recording which step creates an entity would set a clash aside, the agent is asked which one.
  - **Lists:** lists fit the instances they hold.
  - **Kept optionals:** `x = old(x)` keeps an optional value that is unset.
  - **Write inference:** reads of other instances are no longer taken as writes.
  - **Pruning:** keeps what a rule that does not compile yet names.
- **Clearer questions.**
  - **Examples:** a question's example keeps each slot's values ("shed: tools #2: id is 0").
  - **Statuses:** an undecided check reads as `undecided`, not `broken`.
  - **Agent reminders:** the hook reminds the agent of requests the owner has not approved, and asks it to check and report the approved requirements a change touches.
- **Commits and approval.**
  - **Base branch:** in a Shale project, a commit to the base branch waits for the owner's approval of the requests it carries (on a branch, the merge waits).
  - **Staged content:** the pre-commit hook checks the staged specs, not the working tree.
  - **Exact approval:** approval applies exactly the registry the check produced.
  - **Gate:** a head that removes an entity another step wrote no longer panics the gate.
- **Translation.** The coverage judge reads a listing of the items that meet a condition as exactly those. Translation reads a refused step as changing nothing, a record as kept on the step's own instance, and an output of an optional value as optional.
- Watch tests no longer flake on a half-written file.

## v0.1.4 - 2026-11-03

- **A skipped translation is not a one-off task.** A model can answer with placeholders instead of rules ("The requested rule translation is not provided in this response"). The coverage judge now also reads a statement whose readings are all informal: when it finds a clause a rule can check, the statement is translated again, and if it still has no rule it is reported under `failed`, to be retried, instead of being asked about as a one-off task.
- **Shale's evidence rides the code commit.** The agent instructions say to run `shale done` before committing, so the commit carries the request's evidence with the code (Shale 0.2.3 stages it). A Shale session that ends for good closes its request as `shale done` does, and a request still open when a commit recorded the closed ones is read as its own.
- `stele init` adds its check to an existing pre-commit hook (Shale's, a linter's) instead of skipping it, and never rewrites a hook that already runs Stele.

## v0.1.3 - 2026-11-03

- **Each task of a session is its own request.** An agent working through several features in one conversation recorded them in one Shale session, and only the latest request counted: the next feature read as an edit of the approved one before. A completion (`shale done`) now closes a request, and the next one is `intent/<session>-c<n>`, the name Shale 0.2.2 gives its file; a request declared again before the completion still restates it.
- **A one-off task is not a requirement.** With Shale requests as the specs, a request Stele finds nothing to check in ("write the review", "set up the tracker") comes back as a question for the owner: a requirement the product must keep, or a one-off task. A task is set aside in `.stele/retired-intents.yaml`, marked "not a requirement", and leaves the change instead of being approved as a requirement that protects nothing.

## v0.1.2 - 2026-11-03

Found by the developer trial in Codex, continued.

- **A model call that stalls no longer stalls the check.** `stele change` and `stele adopt` limit each model call (`--request-timeout`, 3 minutes by default, as `stele formalize` does); a call that stalls fails and is retried, and a requirement whose calls keep failing is reported under `failed`. A run that still hits its overall `--timeout` prints its JSON with status `failed` and the error, instead of none.
- **Stopping Stele stops the model CLI it runs.** The CLI runs in its own process group, which a SIGTERM or ^C to Stele did not reach; it was left running. Stele now kills it first.
- **Regular expressions in rules compile.** A string such as `"\s*"`, written with one backslash, failed with "bad string"; an escape Go does not know now keeps its backslash, as the regular expression means.
- **The coverage judge knows what `instances(E)` means:** every other instance, never `self`. It had flagged the uniqueness rule `all(t in instances(Task): t.id != self.id)` as forbidding every task.
- A translation run an earlier release finished with failures recorded is started again, so the first check after upgrading retries them.

## v0.1.1 - 2026-11-03

Found by using Stele with Shale on its own repository, and by a developer trial in Codex.

- **`stele check` in a project,** with no registry named, checks every approved requirement on the branch against every other and reports each contradiction as the requirements it involves, their sources and who approved them. Run it after a merge or pull: changes approved on different branches can contradict each other once they meet.
- **A translation that fails is a failure, not a clean check.** When the model backend fails (not logged in, blocked by a sandbox, no network) or gives no valid reading after its repairs, `stele change` and `stele adopt` report status `failed`, list the requirements under `failed` with the error, exit 1, and offer no approval. Before, the check said `clean` and exited 0, and the failure was cached so that a retry never called the model again; a failure is now never cached, also one an earlier release cached.
- **A new request in the same conversation is its own requirement.** A request declared after the session's evidence was finalized reads as `intent/<session>-c2` (Shale's continuation file), so it is checked against the first one instead of being missed.
- The pre-commit hook no longer asks to run `stele adopt` when there is nothing to adopt, such as a Shale project before its first request.
- Approving a Shale request no longer fails with "changed since the check": Shale records each command into the open session, so the check now fingerprints the declared requests rather than Shale's files.
- Retiring a request that was approved earlier on the same branch now removes its rules in the same change; before, the conflict was asked again.
- A second change on a branch whose first change is approved is named `<branch>-2`, so it no longer writes over the approved change's registry files, answers and card.
- When the base branch predates the approved registry (Stele was set up on this branch), the refusal names the commit that added the registry, to compare with until it reaches the base.
- A PR card records a conflict decision without the solver's example situation, which is for the agent explaining it.
- The first change's approval question no longer says it agrees with an approved spec when nothing is approved yet.
- The change report tells the owner to approve by answering the approval question, as the skill does, instead of with `stele approve`.
- `stele change shale --help` prints the help; it used to ask for `--base`.

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
- **Shale requests as specs.** `stele change shale . --base main` checks the requests a branch's agents declared with `shale intent` against every approved one, including a session still open, before the agent edits. `stele init` picks this format in a repository with `.shale/` and no spec folder. A conflict with an earlier request offers to retire it (`.stele/retired-intents.yaml`) or to restate both as one request.
- **Code touches.** `stele touches` lists the approved requirements the branch's code touches, from Grove's parse of the changed files: a function named like a rule's step, or code that is, reads or writes an attribute the rule reads. The post-edit hook says so when that set changes.
- **PR card.** `stele change`, the owner's answers and the pre-commit hook keep `.stele/cards/<change>.json` current: status, requirements added, changed and retired, the owner's decisions, and the approved requirements the code touches. Shale renders it on the PR.
- The watch hook's message names the command that checks pending statements.
