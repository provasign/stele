# Changelog

## Unreleased

- **A check reads its prompt from the cache in every wave.** The vocabulary a check starts from goes in the cached system prompt, and the terms its own earlier waves added go in each request, so repairs and rereads read the same cached prompt instead of writing a new one. Before, every new term changed the prompt: a two-requirement check on v0.1.6 sent a 15,000-token prompt five times and read none of it from the cache. On a two-requirement check with Sonnet it cost $0.12 instead of $0.17, in 40 seconds instead of 48.
- **Translation replies group rules under their requirement,** which names its overrides once. Replies are about 3% shorter per rule. With the cache change, three runs of the 60-requirement held-out set cost $1.12 to $1.21 (before: $1.34 to $1.50), with 12 to 19 clauses unchecked (before: 9 to 24); one of the three left 17 requirements needing vocabulary where the others left 0 and 5.
- **Rereads run in smaller parallel calls.** The request-size limit now counts the reread note, which is longer than its requirement, so the rereads of a 60-requirement set went in four calls of up to 35 seconds instead of two of 72 to 79; the whole read took 3:01 instead of 3:49 to 4:56, for $1.10 with the readings in the earlier range.
- **The synonym check asks about at least 30 pairs per call** instead of 10: a two-requirement check against a 137-attribute vocabulary asked about 25 pairs in three calls, $0.22 of its $1.31 on v0.1.6, to find nothing.
- Tried and not kept, measured on the same set: Sonnet at medium effort cost no less ($1.50); batches of 10 requirements with 8 calls in parallel cost more ($1.43), left 11 requirements needing vocabulary and invented two names for one fact; a reread that names the rules it keeps instead of writing them out saved a quarter of the reread but recovered 7 of 32 requirements where the full rewrite recovers 16 to 25.

## v0.1.7 - 2026-11-06

- **Stele's calls run on the coding agent you are using, and its model.** `translate.llm: agent`, which `stele init` now writes when Claude Code or Codex is installed, resolves at each run to the agent Stele is run from: Claude Code by its environment, Codex and Claude Code by a record the hook keeps under `.stele/cache/` on every prompt and model switch. The model is the one that agent is using at the time (Codex names it in every hook event; Claude Code at session start and on `/model`), else the CLI's own default. Outside any agent, in CI or a shell, `agent` runs on the installed CLI, else an API key's provider. `claude`, `codex`, `anthropic`, `openai` and `cmd` still name one outright, and `translate.model` still pins a model: on a 60-requirement held-out set `claude-sonnet-5-5` read the whole set, rereads included, for $1.34 in 5 minutes where Opus cost $5.03 and 21 minutes of model time, and the same judge scored Sonnet's rules at 111 clauses covered to 55 uncovered against Opus's 122 to 26. Reasoning effort stays Stele's own (the agent's low effort would cut what the rules cover).
- **Stele works under Codex's sandbox.** Stele calls the model through your codex CLI, which cannot start inside the sandbox Codex runs commands in ("failed to initialize in-process app-server client: Operation not permitted"), so every check run from Codex failed. `stele init` now writes `.codex/rules/stele.rules`, which lets `stele change` and `stele adopt` run outside the sandbox once Codex trusts the project, and a check that still runs inside it stops at once and says to run it outside the sandbox, instead of retrying every statement.
- **Stele knows it runs inside Codex without the project's hooks.** Codex marks the commands it runs with its session, so `agent` resolves to Codex even before `/hooks` is reviewed; the model the hook recorded applies only to the session that recorded it.
- **Agents wait for the check.** Codex ran `stele change` in the background, wrote the feature while it ran and stopped it after a stall; the instructions now say a check takes a few minutes, to wait for its JSON, and to edit only after the owner's approval.
- **A reread that runs to the request limit is not sent again;** the statement keeps its earlier reading. On Codex the same reread stalled twice for 280 seconds each.
- **A rule that takes the length of a map is refused with what to write instead** (which keys it holds with `has`, what it holds with `m[k]`). Told only what `len` takes, the model's repairs of a listing written as a map ran to the request limit, in each of two runs on Codex.
- **An answer about a step a later reading no longer has is left out,** instead of stopping the check: a reread named the entity differently, and "the step creates the plot" left the change's registry unloadable (`extend "Plot": no such entity`).
- **`stele init` run again records a spec format set up after it,** such as Shale's requests after `shale init`; Stele did not read them.
- **The translation prompt is 14,000 characters instead of 23,900.** A held-out comparison read as well as before (101 clauses covered to 50 uncovered, against 95 to 49). Low reasoning effort was tried and dropped: it covered 53.
- **The calls of a translation wave read their shared prompt from the cache.** The shared vocabulary and the code's structure are in the system prompt, and before a wave of several calls a small call that asks for nothing writes it to the cache, so the wave's calls read it instead of each writing it. Only an identical system prompt is read from the cache: the same text at the start of the request is written again by every call. On a 60-requirement set with no approved vocabulary the saving is within run-to-run variation, since the model's output is three quarters of a translation's cost; it grows with a project's approved vocabulary.
- **The synonym check describes each attribute once per call,** instead of again in every pair it is in: its prompts are about half as long, and it cost a third less per pair on the 60-requirement set.
- **The agent's reminder about requests waiting for the owner names this session's requests and counts the others,** instead of listing every waiting request each time one is added, and says the command to run once.
- **A relation to many that the model also marks optional loads,** instead of costing a repair call: a relation to many may be empty already.

## v0.1.6 - 2026-11-06

- **A step's rule is about the instance the step names, unless it says otherwise.** When a step's rules tie the instance to a request field of the same name (`request.number = self.number`), a rule on that step that reads the instance and nothing of that request field is read for the named instance only. "A released plot with a holder goes to the first member waiting" was read for every held plot, and clashed with "releasing an unknown number is rejected"; five rereads and a rewording did not settle it.
- **An exception names the requirement in the spec its text names.** "An exception to FR-006 of the waitlist spec (005-waitlist)" overrode the new spec's own FR-006, and the clash the owner had settled as an exception came back.
- **A world fact added after a rejected one gets its own ID.** It took the ID of the last one, and the change's registry no longer loaded ("rule world-main-3 declared twice").
- **A state rule over another entity's instances holds for the other instances a step looks at.** "A member on the waitlist holds no plot" held for the member a step acted on, not for the first member waiting when a plot was released, and the owner was asked whether that member could already hold a plot.
- **Approved steps can be corrected.** A registry approved before Stele learned to tell a value a step reports from one it writes kept those writes, and nothing corrected them: a listing could change any tool, so clashes no step reaches stood and steps went undecided. When a change check meets such a step (the change's rules are on it, or a clash or an undecided check is in it), it asks the agent whether the step changes each value it only shows in its result. The answer "keeps" is recorded in the change's registry (`extend: Op` with `keeps: [...]`), and the approval names it. Found in all three trial registries (every listing), and once in 160 benchmark registries.
- **`stele init` on a Codex-only machine** sets Codex as the translation model: it chose the claude CLI whatever was installed, and every translation failed. It picks the claude CLI, else the codex CLI, else an API key's provider, and says what to install when there is none.
- **`stele init` on a new Spec Kit, OpenSpec or Kiro project** keeps the spec layout in `stele.yaml` when there is no spec yet, instead of reporting it as unreadable and leaving it out.
- **Checks on Codex run at medium reasoning effort** unless `translate.effort` in `stele.yaml` or `--effort` says otherwise. They ran at the user's own Codex default: at high, a six-requirement Spec Kit change took 7 to 20 minutes, calls stalling past the request limit and retried whole; at medium, 3 to 5 minutes with no stall, and readings as good.
- **`stele init` says when Node.js is missing.** The Claude Code and Codex hooks run with `node`; without it they failed and nothing said why.
- **A requirement the model reads with no rules twice is unchecked, not failed.** It held its change back on every check, and the agent offered to reword the spec to suit Stele; now the second reading of the same text goes on to the approval, each clause no rule checks a finding. The first time, `next` says to run the check again rather than to fix the model command.
- **A Codex call that stalls is sent again after 100 seconds,** and the first answer is used; each attempt has the whole request limit. A few calls stall until the 3-minute request limit, and the same request sent again answers in under a minute.
- **A step a change proposed on an earlier check writes what later rules set after it.** The step was read as keeping the value, and the change reported clashes that were not there.
- **A new attribute of an entity is not a change to the step that creates it.** A change that gave plots a holder made adding a plot "write" the holder, and the owner was asked, twice, whether adding a plot changed meaning; no approved rule reads a new attribute.
- **Rules a change adds to an existing step read reports as reports.** "Listing the tools also shows each lent tool's due date" made the listing write the tool's id and lent date: the fix in v0.1.5 covered new steps only.

## v0.1.5 - 2026-11-05

Found by three developer trials (ten features each, built with Claude Code and Stele from the owner's requests) and a code review.

- **The owner's decisions take fewer turns.**
  - **Precedence:** a new request that adds a case to an approved one can take precedence where they meet, both standing; no restating of the whole rule.
  - **Restated requests:** a request restated after the owner retired the earlier wording is the same requirement, edited, with no rename question.
  - **World facts:** precedence over a request no longer weakens the facts about the world it states (a command's text), which had turned them into clashes with unrelated steps.
  - **Denied precedence:** "MUST NOT override FR-001" no longer reads as overriding FR-001.
  - **Changed numbers:** a request whose rule is an approved rule with only its numbers changed ("at most 2" for "at most 3") asks whether it replaces it, however it is worded.
- **More of what a request says is checked, and the rest is named.**
  - **Exact computed values decide:** "the whole days overdue", stated by two bounds, and "the message names the overdue tool" are decided where the solver used to give up.
  - **Results per item decide:** a uniqueness rule makes its attribute a key, so the tools a step reads with the same id are one tool, and results keyed by id have one value each.
  - **Other instances:** state invariants hold for the other instances a rule ranges over.
  - **Partial checks are named:** a reading Stele could not use is a finding of its requirement, so a requirement checked in part says which part is not.
  - **Free settings are asked about:** a threshold read through a new setting no requirement gives a value is a question: the owner says whether a fixed value was meant.
  - **Undecided steps:** the owner can approve a step the solver cannot decide; the registry records it as unchecked and CI's gate accepts only recorded steps.
- **A missed contradiction is found.** "Stewards may have up to 5 tools lent" beside a 3-tool limit read as an error that changes nothing passed, since the refused lend still ran. Being able to do something now means the step runs and changes something it writes.
- **Fewer false contradictions.**
  - **Unreachable states:** a clash in a state no step reaches is set aside more often. Reachability now fixes whether optional values are set and leaves out entities with no creating step. When recording which step creates an entity would set a clash aside, the agent is asked which one. A clash is set aside only when its rules clash in no reachable state, not when the one state the solver sampled is unreachable.
  - **Lists:** lists fit the instances they hold.
  - **Kept optionals:** `x = old(x)` keeps an optional value that is unset.
  - **Write inference:** reads of other instances, and values a step only reports (a key it looks a result up by, a value compared with its result), are no longer taken as writes.
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
  - **Removals:** a change that only removes requirements comes to the approval, which retires their rules; it used to report clean with the rules still live.
- **Caching.** A cached step result re-decides reachability on every check, since another step's change can make its clashing state reachable. A check that was cancelled or ran out of time is never cached.
- **Processes and timeouts.** Solvers share one set of warm z3 processes per binary, so a long-running `stele serve` no longer gains z3 processes with every check. The glossary and code-evidence model calls of `stele change` have the per-request timeout, like translation.
- **Coverage failures are told.** A requirement the coverage check could not judge is a "not judged" finding in the approval, and is judged again on the next check.
- **Translation.** The coverage judge reads a listing of the items that meet a condition as exactly those. Translation reads a refused step as changing nothing, a record as kept on the step's own instance, and an output of an optional value as optional.
- **Grove v0.63.27.** The index folder ignores itself, so `git add -A` no longer commits it; the release also memoizes the Python re-export walk (v0.63.26).
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
