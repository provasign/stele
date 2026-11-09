# Stele documentation

Stele keeps a project's specs consistent: it codifies every spec statement into a checkable rule over one vocabulary, finds statements that contradict each other, settles them with the owner against the code, gates every later spec change, and learns from the owner's answers. Every statement ends checked or reported ([D-049](design-decisions.md)).

Stele was called Spec Registry until 2026-10-24 ([D-033](design-decisions.md)). Planning documents that used to live here (deployment, development process, benchmarking, test plan, open-source plan, roadmap) moved to the private research repository on 2026-10-31.

## Reading order

| # | Document | What it answers |
| --- | --- | --- |
| 01 | [Vision](01-vision.md) | What problem Stele solves, for whom, and how it differs from prior art |
| 02 | [Goals and principles](02-goals-and-principles.md) | What it must do, what it will not do, and the rules every design choice follows |
| 03 | [Architecture](03-architecture.md) | The pipeline as built: import, translate, check the reading, find conflicts, decide, gate, learn |
| 04 | [Data model](04-data-model.md) | The registry: contexts, entities, operations, rules, decisions, open conflicts, lessons |
| 05 | [IR and semantics](05-ir-and-semantics.md) | The expression grammar, rule kinds, precedence, time and aggregates |
| 06 | [Checker](06-checker.md) | Each check, slicing, caching, timeouts |
| 07 | [Translation and grounding](07-translation-and-grounding.md) | Prose to rules: vocabulary, compact translation, coverage and self-checks |
| 08 | [Workflows](08-workflows.md) | A spec change, a conflict with an old rule, adopting existing specs, coding agents, the rule lifecycle |
| 09 | [Enforcement](09-enforcement.md) | The gate, and what binding rules to code will add |
| — | [Domain coverage](domain-coverage.md) | How any kind of project maps onto the model; assurance levels; theory tiers |
| — | [Examples](examples/README.md) | Worked examples: web service, embedded controller, data pipeline, infrastructure policy, CLI tool |
| — | [Prior art](prior-art.md) | Requirements-to-logic tools and papers, and what Stele takes from them |
| — | [Design decisions](design-decisions.md) | The decision log, D-001 to D-064 |
| — | [Glossary](glossary.md) | Terms used across these documents |

## Status, 2026-11-09

| | Built | Planned |
| --- | --- | --- |
| Import | Spec Kit, OpenSpec (with change folders), Kiro, Markdown; requests declared with Shale and kept as requirements (`stele keep`) | |
| Translation | compact translation on a whole-corpus fact vocabulary; clause coverage and scope check; self-contradiction and blocking-rule rereads | |
| Finding conflicts | Z3 checks (response conflicts, dead responses, vacuity); LLM review with solver confirmation (`stele audit`) | |
| Deciding | conflicts settled against the code; owner questions in the coding agent; decision records; adoption baseline | |
| Changes | `stele change` for OpenSpec, Spec Kit, Kiro and Shale requests; a summary in the owner's words; approval pinned to text and reading; ledger; renames kept; `stele export openspec` from kept requests; a card for the pull request | |
| Learning | lessons from the owner's own words, in the registry's git | |
| Agents | `stele init` sets up Claude Code, the one agent supported for now; the setups for Codex, Cursor, Windsurf, VS Code (Copilot), Gemini CLI and opencode stay for projects that recorded them | |
| CI | `stele gate` | a GitHub integration that runs it on every PR |
| Code | code evidence for conflict questions | checking each code change against the rules it touches; generated tests ([09](09-enforcement.md)) |
| No specs | requests declared with Shale, kept as requirements when the owner says they last | proposing requirements from code |

## Conventions

- Rule IDs (`R12`) and decision IDs (`D7`) in examples are stable across documents.
- The running example is the `resources` context: `Resource`, `Hold`, rule R12, and the data-erasure conflict resolved by D7. Domain-specific walkthroughs live in [examples/](examples/README.md).
- "Must", "must not" and "should" are normative in 02 to 09; elsewhere they are plain English.
