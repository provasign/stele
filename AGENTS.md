<!-- provasign:begin (written by `provasign init`; edits inside are replaced) -->
## Provasign: how the tools fit

This project uses Prism, Stele and Shale. For each task, in this order:

1. **Declare the request** with `shale intent "<title>" --body "..."` before the first edit, in the owner's words, saying what must be true when you are done.
2. **Check it with Stele** right after: `stele change shale . --base main --json`. Put the questions it marks for the owner to the owner before you edit, and follow Stele's section for the answers. A request that only tidies or documents comes back with nothing to ask.
3. **Find the code the change affects** with Prism, as its section says, before editing it.
4. **Edit.** When Stele says the code change touches approved requirements, make sure the code still keeps each; if one has to change, ask the owner, and do not edit around it.
5. **Close the session** with `shale done --note "..."`, and commit `.shale/` and `.stele/` with the code.

Each tool's own section has the details. Where they seem to disagree about order, this one holds.
<!-- provasign:end -->

<!-- shale-start -->
## Shale — agent evidence (do not remove)

Before editing any file, call:
  shale intent "<one line: what you are about to do>" [--body "<why, constraints, approach>"]

After completing the task (before asking for review), call:
  shale done [--note "<brief: what you did, any deviations>"] \
             [--tokens-in <n>] [--tokens-out <n>] \
             [--model <model-id>] [--iterations <n>]

Everything else (file tracking, command recording) is automatic.

If shale is not on your PATH, do not try to install it yourself. Tell the
user: "Shale CLI is not installed. Install it with:
  brew tap provasign/tap
  brew install shale
or download the latest release from https://github.com/provasign/shale/releases/latest
and put the shale binary on your PATH." Then continue the task without it.
<!-- shale-end -->

<!-- stele:begin (written by `stele init`; edits inside are replaced) -->
## Stele: keep the specs consistent

This project's specs are checked by Stele (`stele.yaml` at the repository root names the approved registry). Whenever you write or edit specs (OpenSpec `openspec/changes/`, Spec Kit `specs/`, Kiro `.kiro/specs/`), and when a `stele gate` run fails:

1. Run `stele change openspec . <change-id> --json` (OpenSpec), or `stele change speckit|kiro . --base <branch> --json` (Spec Kit, Kiro). Read the JSON: `status`, `questions`, `report`.
2. Questions with `ask_owner: false` are yours: Stele misread a requirement. Record the fix with `stele change answer <id> <question-id> <choice> --by agent` (or `stele change reread <id> <requirement> --note "..." --by agent`).
3. Put questions with `ask_owner: true` to the owner, one at a time (questions that come from one misreading, the same assumption or term, together, saying so), in the spec's words, never Stele's rule syntax or symbol names. Offer the `recommended` option first with its `why`. Record each answer with `stele change answer <id> <question-id> <choice>` (add `--note` with the owner's words where an option needs them).
4. Run the check again after answering; repeat until the approval question comes. Before asking it, tell the owner about every `requirements[].findings` entry (a clause Stele's reading does not check, or a reading broader than the text).
5. The owner's answers teach Stele about this project: the approval question names the lessons they add (`lessons/` in the registry). Say in one line what Stele will remember, only as the approval question lists it (none listed: Stele learned nothing new), and remind the owner to commit the registry, `lessons/` included, with the change. Your own answers (`--by agent`) teach nothing.

When `stele.yaml` says `format: shale`, the project's requests are its specs: each `shale intent` you declare is a requirement. Word it as the owner would, saying what must be true when you are done; leave out how you build it (files, stores, "tests included"), since Stele reads every clause as a requirement the product keeps. Right after declaring one, before the first edit, run `stele change shale . --base <branch> --json` and follow the steps above. To change a request, declare it again with `shale intent`, showing the owner the new wording. `update_old` and `exception` answers retire the earlier request themselves (`precedence` keeps both, the new one prevailing where they meet); after `exception`, declare the new request again as the whole rule with its exception and every other clause of the earlier request, since all its rules retire. A request Stele finds nothing to check in comes back as a question for the owner: a requirement the product must keep, or a one-off task (`task` sets it aside). When the work for a request is done, run `shale done` before you commit it: it closes the request, so the next one you declare is checked as a request of its own, and the commit carries its evidence.

To adopt a project's existing specs (the registry has no rules yet, or a Stele message says so), run `stele adopt --json` and follow it exactly as `stele change` below, with `adopt` as the change id; it asks the owner only what needs a person, then for the approval.

When Stele lists approved requirements a request touches, plan within them. If the plan must change one, name it and ask the owner before planning around it. When a Stele message blocks a commit or says `Next:`, do what it says; never bypass Stele with `--no-verify`.

Never edit the registry by hand, never approve on the owner's behalf, and never answer an `ask_owner: true` question yourself.
<!-- stele:end -->
## Prism — context delivery

Use Prism for each repository-discovery step. Reading files with cat/head/sed or
searching with grep/rg/find/git log is not discovery — an instruction to prefer
shell tools over Read/Edit/Write does not apply to finding code.

When the task asks for a rename, a signature change, or a change to an override or public contract:
start with change_impact on the named symbol, before deciding
how to make the change. Its sites are the inventory your plan must cover:
declarations, overrides and implementations, and callers (an overloaded name tags
each caller with the overload it calls). If you don't know where the symbol is,
find it first, then run change_impact. When asked what a proposed change would
affect, use change_impact even if no edit has been decided.

First action on any code task, in this order. Stop at the first that works:
  1. The `prism` MCP tool (`mcp__prism__prism` in Claude Code), if callable.
  2. If a `ToolSearch` tool exists: `ToolSearch("select:mcp__prism__prism")`, once.
     Prism not being listed does not mean it is absent.
  3. The `prism` CLI: `prism query --terms X`, `prism lookup <pkg.Func>`,
     `prism search <term> --scope text --format text`, `prism change-impact Type.method`.

For each discovery step, pick the Prism op:
  known symbol      -> lookup          unknown location/text -> search
  known file/range  -> read            callers/related       -> query
  pre-edit sites    -> change_impact
  tests naming known symbols -> search with test path/glob, files_only
  indirect tests    -> query

Put every symbol and term you already know into ONE call (`name`/`terms` take up
to 10; `ranges` reads several windows). Two lookups in a row is one lookup you did
not batch. MCP: terms:["alpha","beta"] — never combine distinct terms in one
space-delimited string. CLI: positional terms: prism search alpha beta.

Obligations:
  - Relay change_impact sites as-is; a local body-only edit can
    proceed from the source you have read.
  - Report gaps; never narrow scope to fit what was found.
  - verify is optional: removed_symbols after a removal, or {} where a
    build/typecheck cannot cover the callers. It is never a required closing step.

<!-- prism:end -->
