<!-- provasign:begin (written by `provasign init`; edits inside are replaced) -->
## Provasign: how the tools fit

This project uses Prism, Stele and Shale. For each task, in this order:

1. **Declare the task** with `shale intent "<title>" --body "..."` before the first edit, in the owner's words. Add `--for <change-id>` when it implements a spec change.
2. **Keep it only if it is a requirement.** A request is a task, and Stele says nothing about it. When the owner asks for something the product must keep from now on, run `stele keep --by agent`, then `stele change shale . --base main --json`, and put the questions it marks for the owner to the owner before you edit, following Stele's section for the answers. A one-off job (a review, a trial, a fix, a write-up) is never kept. If a task turns out to state a lasting rule, offer once, in one line when the work is done, to keep it; on no, `stele keep <request> --decline`.
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
