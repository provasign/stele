---
name: stele-change
description: Check a spec change (an OpenSpec change folder, a Spec Kit or Kiro feature branch, or a request declared with `shale intent`) against the project's approved spec with Stele before it is applied, and put the decisions it needs to the owner as short questions. Use after writing or editing specs (openspec/changes/, specs/, .kiro/specs/), when the user asks to check, validate or approve a change, or when a Stele gate fails in CI.
---

# Check a spec change with Stele

Stele translates the requirements a change adds or edits into checkable rules and checks them against every approved requirement. You run it, read its JSON, and ask the owner only what needs a person. The owner never reads Stele's files, rule syntax or symbol names: you speak in the spec's words.

## Setup

- `stele` must be on PATH. `stele.yaml` at the repository root names the approved registry (`registry:`) and the model (`translate.llm`, `translate.model`); `stele change` uses them. Without `stele.yaml`, pass `--registry <dir>` and ask the owner once where the registry is.
- A check that reuses earlier translations costs nothing; one that translates usually costs a few cents to a few tens of cents. It prints `cost_usd`.

## Adopting existing specs

When the registry has no rules yet (a new Stele project with specs, or a Stele message says the specs are not adopted), run:

```sh
stele adopt --json
```

It reads every requirement once and checks them against each other. Its JSON is `stele change`'s, with the change id `adopt`. Follow the loop below from step 2, recording answers with `stele change answer adopt <question-id> <choice>` and checking again with `stele adopt --json`. The approval writes the registry and the accepted specs; tell the owner to commit them.

## Loop

1. Find the change and run the check:
   - **OpenSpec:** the change folder you just wrote, or `stele change openspec . --list`:

     ```sh
     stele change openspec . <change-id> --json
     ```

   - **Spec Kit (`specs/`) or Kiro (`.kiro/specs/`):** the change is what the current branch changes in the specs, compared with the branch it will merge into, the one that holds the approved registry (usually `main`; if more than one could be the base, pick the one where `git ls-tree --name-only <branch> stele.yaml` finds `stele.yaml`, or ask the owner once):

     ```sh
     stele change speckit . --base main --json     # or: stele change kiro . --base main --json
     ```

     The change is named after the branch (`<branch>-2` and on once a change of that name is approved); use the `change` field of the JSON as `<change-id>` below.
   - **Shale requests** (`stele.yaml` has `format: shale`): each `shale intent` is a requirement. Right after you declare one, before the first edit, check the branch's requests:

     ```sh
     stele change shale . --base main --json
     ```

     The base is chosen as for Spec Kit. A request that only tidies or documents (no behaviour to check) comes back unchecked, and nothing is asked about it.
2. Read the result. If the command fails without printing JSON (a configuration or setup error), report the error to the owner in one line and stop: it is not a question for them.

   Progress goes to stderr; the JSON on stdout has `status`, `next` and `questions`.
3. Act on `status`:
   - `clean`: nothing to decide. Tell the owner in one line.
   - `approved`: the owner already approved this change as it reads. Do not check it again; commit the registry with the change.
   - `broken`: the change's rules do not compile together (`errors`). This is Stele's problem, not the owner's: report it briefly and stop.
   - `failed`: the model could not translate the requirements under `failed`, so nothing was checked for them; the error says why (not logged in, a sandbox that blocks the model command, no network). Never report this as checked. Tell the owner the error in one line and what would fix it, and run the same command again once it is fixed: the failures are retried, not reused.
   - `questions`: go to step 4.
   - `ready`: only the approval question is left. Go to step 6.
4. For each question, in order:
   - If `ask_owner` is false, answer it yourself; these are about Stele's modelling, not the owner's intent:
     - `self_conflict`: answer `retranslate`. Stele misread a requirement and reads it once more before the owner is asked.
     - `same_term` with `ask_owner` false: both names are Stele's, for things this change's own requirements describe. Read the requirements and answer `same` if they describe one thing, else `different`.
     - `creates`: decide from the spec and the code whether the step only ever makes a new instance. Answer `creates` only if nothing lets it run on an existing one: look for scenarios such as re-initializing, re-running or updating, and answer `existing` if there are any. If you cannot tell, ask the owner.
   - Otherwise ask the owner. Use `text` as the question, nearly verbatim (put an example situation such as "pager: launch runs and pager: spawned" in plain words: "when the pager is launched and starts"), and `options[].label` as the choices. When the question has `recommended`, offer that option first, marked as recommended, with `why` as its description. When it has `code`, Stele has read the project's code for that situation: tell the owner in one plain sentence what the code does now (`code.says`, shortened; name the file and line from `code.cites`), before the options. If `code.outcome` is `not_implemented`, say the code does neither yet. The code shows what the project does, not what the owner wants: the owner may still choose otherwise. In Claude Code use the AskUserQuestion tool; elsewhere ask in chat with numbered options. Do not show rule syntax, rule IDs or symbol names such as `Cluster.flushRecorded`; quote a requirement's `text` only if the owner asks what it says. Ask related questions together, at most four at a time.
   - Record the answer:

     ```sh
     stele change answer <change-id> <question-id> <choice> [--note "<the owner's words>"]
     ```

     Options with `needs_note` need the owner's words: ask what Stele got wrong and pass it as `--note`. Any other answer may carry the owner's explanation as `--note` too (for "that situation cannot happen", why it cannot). Record the answers you give yourself (`ask_owner` false) with `--by agent`; the owner's answers use the default, their git name.
   - Do what the printed `Next:` says. When you write an exception into the spec, name the requirement it takes precedence over by its label or title ("regardless of `--color` (FR-003b)"), so Stele can record the precedence. For `change`, `update_old`, `exception` and `reword`, you edit the change's spec files yourself, showing the owner the new wording before saving it. In OpenSpec, edit only the change folder (a MODIFIED or REMOVED requirement for `update_old`), never `openspec/specs/`. In Spec Kit and Kiro, edit the spec files on the branch. For Shale requests, never edit `.shale/`: for `change`, declare the request again with `shale intent "<title>" --body "..."`; for `exception`, the answer retires the earlier request, and you declare this one again stating the whole rule with its exception ("at most 5 books, or up to 10 during the summer program"); `update_old` needs no edit, since the answer retires the earlier request. Commit `.stele/retired-intents.yaml` with the change. Never edit the registry.
5. Once every question of the round is answered and recorded (yours and the owner's), run the check again if any `Next:` said to; one run covers them all. Earlier translations of unchanged requirements are reused, so this is cheap. Repeat until the status is `ready` or `clean`.
6. Before the approval, review Stele's readings. Stele already checks each reading against its text and has read a flagged requirement once more: what it still finds is in the approval question's `requirements[].findings` (a clause no rule checks, a rule broader than the text, a rule that blocks a step), beside `requirements[].reading` and `requirements[].text`. Also compare each reading with its text yourself. Where a reading says less than the text (a MUST read as a MAY, a missing case such as "whether or not stdout is a terminal") or more (a condition dropped, so it applies more widely), and Stele's finding or your own reading says so, record a re-read yourself, quoting the text:

   ```sh
   stele change reread <change-id> <requirement> --note "<what the text says that the reading misses>" --by agent
   ```

   Then check again; handle any new questions as in step 4. Re-read a requirement at most once yourself: if it still reads wrong, tell the owner in the approval question. This is Stele's reading, not the owner's intent, so it needs no question; but when you ask for the approval, say in one line which requirements you had re-read and why.
7. The approval: ask the `approve` question. When any requirement has findings, the question recommends `show`: say which requirements are flagged and why, in the spec's words, before offering the choices. If the owner picks `show`, summarise each requirement's reading (`requirements[].reading`) in one plain sentence beside its text, and state every finding plainly ("Stele's reading does not check that erasure requests are exempt"). The owner approves the reading, not the text, so they must see what the reading leaves out. If the owner says a reading is wrong, record it, which needs no edit to a correct spec, then check again:

   ```sh
   stele change reread <change-id> <requirement, e.g. FR-012> --note "<what Stele got wrong, in the owner's words>"
   ```

   Then ask the approval again. After any reread, edit or answer, the approval question comes back with a new `id`: always use the ids from the latest check. `not_yet` records nothing. `approve` writes the change's rules into the registry and records the approval. Tell the owner to commit the registry with the change.

## After an OpenSpec change is archived

`openspec archive` moves an approved change into `openspec/specs/`. Then run `stele sync --apply`: it points the rules at the requirements' new place and records the specs as accepted, changing no meaning. Commit what it changes with the archive.

## What Stele learns

The owner's answers teach Stele about the project: "the same thing", "that situation cannot happen", and what Stele misread are saved as lessons in the registry's `lessons/` (the result's `new_lessons`), and every later check is read with them. When the approval question names lessons, say in one line what Stele will remember. Remind the owner to commit `lessons/` with the registry. If the owner tells you something about the project no question asked ("on this project, a VM is a Node"), record it: `stele lesson add --kind term|fact|scope --says "<their words>"`. Answers you give yourself (`--by agent`) teach nothing, by design.

## Rules

- Never answer a question with `ask_owner: true` yourself, and never pass the owner's name with `--by` for an answer you gave. When the owner gives an answer or an approval, record it without `--by`: Stele takes their identity from git, never from your own context.
- Never use `stele approve` or `--apply` directly in this loop; `stele change answer … approve` does both after checking the check is current.
- If CI's `stele gate` fails with unapproved rules or conflicts, run this loop for the change in that branch.
- Requirements listed under `unchecked` were not checked. Mention how many when asking for approval (the question already says so); do not try to fix them.
