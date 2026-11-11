---
name: stele-change
description: Check a spec change (an OpenSpec change folder, a Spec Kit or Kiro feature branch, a requirement written in the project's requirements folder, or a request declared with `shale intent`) against the project's approved spec with Stele before it is applied, and put the decisions it needs to the owner as short questions. Use after writing or editing specs (openspec/changes/, specs/, .kiro/specs/, requirements/), when the user asks to check, validate or approve a change, or when a Stele gate fails in CI.
---

# Check a spec change with Stele

Stele translates the requirements a change adds or edits into checkable rules and checks them against every approved requirement. You run it, read its JSON, and ask the owner only what needs a person. The owner never reads Stele's files, rule syntax or symbol names: you speak in the spec's words.

## Setup

- `stele` must be on PATH. `stele.yaml` at the repository root names the approved registry (`registry:`) and the model (`translate.llm`, `translate.model`); `stele change` uses them. Without `stele.yaml`, pass `--registry <dir>` and ask the owner once where the registry is.
- A check that reuses earlier translations costs nothing; one that translates usually costs a few cents to a few tens of cents. It prints `cost_usd`.
- A check that reads new requirements takes a few minutes, and a large one more than one run: it then returns `continue`, and you run it again. Wait for it to finish and read its JSON before any edit; do not work on the code while it runs, since its questions can change what the code must do.
- Run `stele change` and `stele adopt` as written, without pipes or redirection, and read the JSON they print. Stele calls the model through your Claude Code CLI.

## Adopting existing specs

When the registry has no rules yet and the project has specs in OpenSpec, Spec Kit or Kiro (a Stele message says the specs are not adopted), run:

```sh
stele adopt --json
```

It reads every requirement once and checks them against each other. Its JSON is `stele change`'s, with the change id `adopt`. Follow the loop below from step 2, recording answers with `stele change answer adopt <question-id> <choice>` and checking again with `stele adopt --json`. The approval writes the registry and the accepted specs; tell the owner to commit them.

On a project of more than a hundred requirements, `stele adopt --json` reads nothing at first: it answers with status `choose`, the specs with their sizes and what reading each takes (`specs`). Tell the owner the count and ask which specs to start with, then run `stele adopt --json --spec <name>[,<name>]`, with the same `--spec` on every later run of that adoption; `stele adopt --json --all` reads everything at once, for an owner who wants that, and takes long enough to need the background or a 30-minute timeout. The summary names the specs not read yet; a later `stele adopt --json --spec <name>` reads more of them, checked against what is approved. Until a spec is read, Stele checks nothing in it, but a change that edits one of its requirements is still checked as a change.

At an adoption Stele settles by itself what is about its own rules and not about the specs: you get no `creates`, `creator` or `self_conflict` questions, and a clash of its rules that a reading of the texts does not show is recorded as open or settled by precedence, counted in the summary and the approval. The questions that reach the owner are contradictions a reading of the texts also finds.

At an adoption, a conflict question also offers `later`: the contradiction is recorded as open, listed at the approval and by `stele check`, and the adoption goes on. When more than three conflicts wait (the `next` text says so), tell the owner once that they can decide any of them later, and ask the ones they want to settle now; never answer `later` for them. When both requirements are being adopted (or both are in the change), the question also offers `prevails` and `prevails_other`: one requirement is an exception to the other, recorded with no edit to the spec and no new reading. Stele recommends it when one is an error case or a check before the step ("verify that the member exists before lending" beside "SHALL record the loan"); offer it in the labels' words, and never pick it for the owner.

## Loop

1. Find the change and run the check, as soon as the draft is written and before you show the spec to the owner, so the check runs while you finish. Where Shale is installed, declare the task first with `shale intent "<title>" --for <change-id>`, naming the spec change it implements:
   - **OpenSpec:** the change folder you just wrote, or `stele change openspec . --list`:

     ```sh
     stele change openspec . <change-id> --json
     ```

   - **Spec Kit (`specs/`) or Kiro (`.kiro/specs/`):** the change is what the current branch changes in the specs, compared with the branch it will merge into, the one that holds the approved registry (usually `main`; if more than one could be the base, pick the one where `git ls-tree --name-only <branch> stele.yaml` finds `stele.yaml`, or ask the owner once):

     ```sh
     stele change speckit . --base main --json     # or: stele change kiro . --base main --json
     ```

     The change is named after the branch (`<branch>-2` and on once a change of that name is approved); use the `change` field of the JSON as `<change-id>` below.
   - **Requirements in Markdown** (`stele.yaml` has `format: markdown`; a project with no spec tool keeps them under `requirements/`): you write them. Write one only when the owner asks for something the product must keep from now on (a behaviour, a default, a limit), never for a one-off job (a review, a trial, a write-up, a fix, a measurement); a request worded as a job ("add a return command; only the holder can return a packet") is a task even when it names a rule: build it, then offer once, in one line, to write the rule down. Each requirement is one list item in the file for its area (`requirements/<area>.md`), starting with an ID no other requirement has (`REQ-001`) and saying what must be true with MUST, MUST NOT or SHOULD, worded as the owner would, never how it is built. Then, before the first edit of code:

     ```sh
     stele change markdown . --json
     ```

     The change is what the folder holds that the owner has not approved, so it needs no base branch and the project's first requirements are checked the same way: there is nothing to adopt. It is named after the branch; use the `change` field of the JSON as `<change-id>` below. To change or drop a requirement, edit or delete its line and check again.
   - **Shale requests** (`stele.yaml` has `format: shale`): a `shale intent` is a task, what you are about to do, and Stele says nothing about it. Only a request kept as a requirement is a spec. Keep one only when the owner asks for something the product must keep from now on (a behaviour, a default, a limit), never for a one-off job (a review, a trial, a write-up, a fix, a measurement). Word it as the owner would, saying what must be true, and leave out how you build it (file names, stores, budgets, "tests included"): Stele reads every clause as a requirement the product keeps. Right after you declare it, before the first edit, keep it and check the branch's kept requests:

     ```sh
     stele keep --by agent
     stele change shale . --base main --json
     ```

     `stele keep` keeps the newest request; name another by its ID or the start of its title, and `--decline` sets a request aside for good when the owner says it is not a requirement, and `--drop` makes a kept request a task again. Keep a request before you build only when the owner says it lasts ("from now on", "always", "never", or a default or limit the product keeps); a request worded as a job ("add a return command; only the holder can return a packet") is a task even when it names a rule. When a task you did not keep states a rule the product keeps (who may do something, a limit, a default), offer it once, in one line, when the work is done: keep it as a requirement so Stele guards it, or not. Yes: `stele keep <request> --by agent`; no, or no answer: `stele keep <request> --decline`, never offered again. `stele keep --list` shows which requests are kept, set aside, or tasks never offered. `stele export openspec` turns a project's kept requests into its first OpenSpec spec when it adopts the tool. Commit `.stele/kept-intents.yaml` with the change. The base is chosen as for Spec Kit. A kept request with no behaviour to check comes back with one question for the owner. Put it as Stele words it, with both answers and what each does: a one-off job, which `task` sets aside; or a requirement the product keeps from now on, which Stele records in the spec without checking it.
2. Read the result. If the command fails without printing JSON (a configuration or setup error), report the error to the owner in one line and stop: it is not a question for them.

   Progress goes to stderr; the JSON on stdout has `status`, `summary`, `next` and `questions`. The command exits 1 whenever something is left to decide or approve: with JSON printed, that is not a failure. The same JSON is saved as `summary.json` in the change's folder (`.stele/change/<change-id>/`): read it there, with your file-reading tool, if your tool cut the output short. Never run the check again only to read its result, and never through a pipe. `summary` is the check in the owner's words: show the spec with it underneath and the questions in one round, so the owner approves the spec and Stele's reading in one act.
3. Act on `status`:
   - `clean`: nothing to decide. Tell the owner in one line.
   - `continue`: Stele stopped itself before your command's time limit, with its work kept. Run the same command again at once, and tell the owner in one line that it is still reading and how far it is (`summary`). Do not wait, and do not raise a timeout.
   - `choose` (an adoption of a large project): nothing was read. Put the choice in `summary` and `specs` to the owner, as `next` says.
   - `approved`: the owner already approved this change as it reads. Do not check it again; commit the registry with the change.
   - `broken`: the change's rules do not compile together (`errors`). This is Stele's problem, not the owner's: report it briefly and stop.
   - `failed`: the model could not translate the requirements under `failed`, so nothing was checked for them; the error says why (not logged in, a sandbox that blocks the model command, no network). Never report this as checked. Tell the owner the error in one line and what would fix it, and run the same command again once it is fixed: the failures are retried, not reused.
   - `undecided`: the solver could not settle what a step must do, and other questions are open. Answer them first. The approval then says which step it leaves unchecked: tell the owner, and offer a simpler wording of the requirement behind it if they would rather have it checked.
   - `questions`: go to step 4.
   - `ready`: only the approval question is left. Go to step 6.
4. For each question, in order:
   - If `ask_owner` is false, answer it yourself; these are about Stele's modelling, not the owner's intent:
     - `self_conflict`: answer `retranslate`. Stele misread a requirement and reads it once more before the owner is asked.
     - `conflict` with `ask_owner` false: the code does what both requirements say in that situation, so Stele most likely read one of them too widely. Answer `misread` with `--by agent` and, as `--note`, what the code does (the question's `why`). Stele reads them again and asks the owner only if they still clash.
     - `same_term` with `ask_owner` false: both names are Stele's, for things this change's own requirements describe. Read the requirements and answer `same` if they describe one thing, else `different`.
     - `creates`: decide from the spec and the code whether the step only ever makes a new instance. Answer `creates` only if nothing lets it run on an existing one: look for scenarios such as re-initializing, re-running or updating, and answer `existing` if there are any. If you cannot tell, ask the owner.
     - `creator`: Stele asks which step makes a new instance of something. Name the step only if the spec or the code shows it never runs on an existing one; answer `none` when instances exist from the start, come from outside, or every listed step also runs on existing ones. Answer it consistently with any `creates` question about the same step.
   - A `free_setting` question is the owner's: a requirement reads a value no requirement fixes (a threshold, a count shown from data). Ask whether it is one fixed value, which Stele then checks the rules against, or a value that is configured or comes from the data. Offer no answer first unless the question recommends one.
   - Otherwise ask the owner. When several questions wait (the `next` text says how many), first tell the owner in one sentence how many there are and what they are about. Use `text` as the question, nearly verbatim (put an example situation such as "pager: launch runs and pager: spawned" in plain words: "when the pager is launched and starts"), and `options[].label` as the choices. When the question has `recommended`, offer that option first, marked as recommended, with `why` as its description. When it has `code`, Stele has read the project's code for that situation: tell the owner in one plain sentence what the code does now (`code.says`, shortened; name the file and line from `code.cites`), before the options. If `code.outcome` is `not_implemented`, say the code does neither yet. The code shows what the project does, not what the owner wants: the owner may still choose otherwise. In Claude Code use the AskUserQuestion tool; elsewhere ask in chat with numbered options. Do not show rule syntax, rule IDs or symbol names such as `Cluster.flushRecorded`; quote a requirement's `text` only if the owner asks what it says. Ask related questions together, at most four at a time.
   - Record the answer:

     ```sh
     stele change answer <change-id> <question-id> <choice> [--note "<the owner's words>" --quoted]
     ```

     Options with `needs_note` need the owner's words: ask what Stele got wrong and pass it as `--note`. Any other answer may carry the owner's explanation as `--note` too (for "that situation cannot happen", why it cannot). Add `--quoted` only when the note is the owner's own words copied as they said them, with nothing added (never a paraphrase, a summary or a prefix such as "Owner:"): only such a note becomes a lesson. A choice the owner names in a word ("Exception.") needs no note. When the owner answered with a choice alone ("1", "yes") and you write the note, leave `--quoted` off: your note guides this check and teaches Stele nothing. Record the answers you give yourself (`ask_owner` false) with `--by agent`; the owner's answers use the default, their git name.
   - Do what the printed `Next:` says. When you write an exception into the spec, name the requirement it takes precedence over by its label or title ("regardless of `--color` (FR-003b)"), so Stele can record the precedence. For `change`, `update_old`, `exception` and `reword`, you edit the change's spec files yourself, showing the owner the new wording before saving it. In OpenSpec, edit only the change folder (a MODIFIED or REMOVED requirement for `update_old`), never `openspec/specs/`. In Spec Kit and Kiro, edit the spec files on the branch. For Shale requests, never edit `.shale/`: for `change`, declare the request again with `shale intent "<title>" --body "..."`; for `exception`, the answer retires the earlier request with all its rules, and you declare this one again stating the whole rule with its exception ("at most 5 books, or up to 10 during the summer program") and every other clause of the earlier request; say so to the owner before they choose it; `update_old` needs no edit, since the answer retires the earlier request; `precedence` (both requests stand, the new one prevailing where they meet) needs none either, since Stele reads the new request again with the precedence, and it is usually lighter than `exception` when a request adds a case to an approved one; `task` (the owner says a request is a one-off task, not a requirement) needs none either, since the answer sets it aside. Commit `.stele/retired-intents.yaml` with the change. When the work for a Shale request is done, run `shale done` before committing: it closes the request (the next one you declare is then a request of its own, not a rewording of this one) and stages its evidence for the commit. Never edit the registry.
5. Answer your own questions first and run the check again, until none with `ask_owner` false is left: your answers can settle, or raise, questions for the owner, who should get theirs in one round. Then, once every question of the round is answered and recorded (yours and the owner's), run the check again if any `Next:` said to; one run covers them all. Earlier translations of unchanged requirements are reused, so this is cheap. Repeat until the status is `ready` or `clean`.
6. Before the approval, review Stele's readings. Stele already checks each reading against its text and has read a flagged requirement once more: what it still finds is in the approval question, beside `requirements[].reading` and `requirements[].text`. `requirements[].findings` are what the owner decides on: a rule broader than the text, or a rule that blocks a step. `requirements[].not_checked` are the clauses no rule checks: the reading is weaker there, and nothing the owner answers changes it. Also compare each reading with its text yourself. Where a reading says less than the text (a MUST read as a MAY, a missing case such as "whether or not stdout is a terminal") or more (a condition dropped, so it applies more widely), and Stele's finding or your own reading says so, record a re-read yourself, quoting the text:

   ```sh
   stele change reread <change-id> <requirement> --note "<what the text says that the reading misses>" --by agent
   ```

   Then check again; handle any new questions as in step 4. Re-read a requirement at most once yourself (Stele refuses a second `--by agent` reread of it, and a second `misread` answer from you): if it still reads wrong, tell the owner in the approval question. This is Stele's reading, not the owner's intent, so it needs no question; but when you ask for the approval, say in one line which requirements you had re-read and why.
7. When the JSON has `possible_conflicts` (an adoption's requirements that may contradict each other by their texts, though Stele's rules do not show it), tell the owner each one before the approval, in a sentence: the requirements by their `titles`, the `situation`, and that Stele could not prove it (`why`). These are not questions and need no answer: the owner may reword a requirement, or approve knowing of them. The approval is one act for the owner. `next` names the command that records it, with the approval question's `id`:

   ```sh
   stele change answer <change-id> <approve-question-id> approve
   ```

   First show the owner the spec with `summary` under it. When a requirement has `findings`, say which and why, in the spec's words ("Stele's reading also forbids deleting a resource whose holds are old; the text says recent holds"), and offer to show the readings: the owner approves the reading, not the text, so they must see where it says more than the text. When a requirement is checked in part, name each clause no rule checks in one line, in the spec's words (`requirements[].clauses` with `checked: false`, and its `why`): the owner approves a reading that leaves those parts of the text unchecked, so they must see which. With no findings, add nothing to the summary. Then:
   - **Where `next` says Claude Code asks the owner** (Stele's hook is installed): run the command. Claude Code shows the owner the summary and asks them to allow it; their yes is the approval. Do not ask in chat as well. If they deny it, nothing is recorded: ask what they want changed.
   - **Where the hook refuses the command, or it is held with no answer from the owner** (Claude Code is not asking for permissions in this session, or runs with no one at the prompt): ask the `approve` question in chat, and when the owner says yes, run the command again with `--asked` added. Never add `--asked` before they answer.
   - **With no hook:** ask the `approve` question in chat, and run the command when the owner says yes.

   If the owner asks how Stele read the requirements (the `show` option), show each requirement clause by clause (`requirements[].clauses`: the clause, and what checks it in words or that nothing does; `in_words` has each rule, `reading` the same in rule syntax) beside its text, and state every finding and every clause not checked plainly ("Stele's reading does not check that erasure requests are exempt"). Read the rules as the report's legend says (`instances(E)` is every other instance, never `self`, so `count(p in instances(Packet): ...) <= 2` allows three with this one); a rule that only looks wrong to you is no reason to warn the owner or to reread: the findings say where a reading is narrower or broader than the text. If the owner says a reading is wrong, record it, which needs no edit to a correct spec, then check again:

   ```sh
   stele change reread <change-id> <requirement, e.g. FR-012> --note "<what Stele got wrong, in the owner's words>" --quoted
   ```

   Leave `--quoted` off when the note is your summary rather than the owner's words.

   Then come back to the approval. After any reread, edit or answer, the approval question comes back with a new `id`: always use the ids from the latest check. `not_yet` records nothing. `approve` writes the change's rules into the registry and records the approval. Tell the owner to commit the registry with the change.

## After an OpenSpec change is archived

`openspec archive` moves an approved change into `openspec/specs/`. Then run `stele sync --apply`: it points the rules at the requirements' new place and records the specs as accepted, changing no meaning. Commit what it changes with the archive.

## What Stele learns

The owner's answers teach Stele about the project: "the same thing", and the owner's own words (`--quoted`) for "that situation cannot happen" and what Stele misread, are saved as lessons in the registry's `lessons/` (the result's `new_lessons`), and every later check is read with them. When the approval question names lessons, say in one line, in plain words and without its ID, what Stele will remember; describe only the lessons it lists. When it names none, leave lessons out of what you tell the owner. Remind the owner to commit the registry, `lessons/` included. If the owner tells you something about the project no question asked ("on this project, a VM is a Node"), record it: `stele lesson add --kind term|fact|scope --says "<their words>"`. Answers you give yourself (`--by agent`) teach nothing, by design.

## Rules

- Never answer a question with `ask_owner: true` yourself, and never run the approval command for a change the owner has not seen: the permission prompt is their approval, not a formality to get past. Never pass the owner's name with `--by` for an answer you gave. When the owner gives an answer or an approval, record it without `--by`: Stele takes their identity from git, never from your own context.
- Never use `stele approve` or `--apply` directly in this loop; `stele change answer … approve` does both after checking the check is current.
- If CI's `stele gate` fails with unapproved rules or conflicts, run this loop for the change in that branch.
- Requirements listed under `unchecked` were not checked. Mention how many when asking for approval (the question already says so); do not try to fix them.
