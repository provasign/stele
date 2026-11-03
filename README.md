# Stele

*Specs written months apart should not be allowed to disagree.*

Stele keeps a project's specs consistent with each other. It reads the requirements a project already writes (Spec Kit, OpenSpec, Kiro or plain Markdown), or, in a project without spec documents, the requests its coding agents declare with [Shale](https://github.com/provasign/shale). It turns each one into a checkable rule over one shared vocabulary, and finds requirements that contradict each other, however far apart they were written. Each contradiction comes back as a question for the project's owner, with a concrete example and what the code does today. From then on, every spec change is checked against every approved requirement before it merges, and what the owner decides is remembered in the repository.

It works inside the coding agent the project already uses. The agent runs Stele when it edits specs and asks the owner only what needs a person.

**Status:** experimental. The commands below work end to end on real repositories; measured accuracy is uneven, and is stated in [what to expect](#what-to-expect). Stele is part of the [Provasign](https://provasign.dev) tool family and stands alone: one `stele` binary, with Z3 for the solver.

## Why

Spec-driven projects accumulate specs. Each is reviewed against whatever the reviewer remembers, and nothing re-reads last spring's spec when this autumn's lands. Coding agents then implement whatever they read. The case studies found this in each major spec format:

| Project | Format | What happened |
| --- | --- | --- |
| Project A | Spec Kit | 5 contradicting `MUST`s in workspace lifecycle; premature shutdowns and workspaces left running |
| Project B | OpenSpec | Delta merges kept old requirements beside new ones; a resume path skipped a required check |
| Project C | Kiro | A new access-role spec contradicted an older user-management spec; the feature was unusable in production |

## How it works

1. **Import.** `stele import` extracts every normative statement, with its source location.
2. **Translate.** Statements become rules over one vocabulary proposed for the whole corpus first, so one fact gets one name. A requirement Stele cannot express is reported, not dropped.
3. **Check its own reading.** Stele asks whether each rule covers every clause of its statement and no more, rereads statements whose rules contradict each other or block a step, and names what remains.
4. **Find conflicts.** An LLM review of the whole corpus proposes contradictions; the SMT solver checks them against every approved rule and confirms those it can express, each with a situation in which the rules cannot all hold.
5. **Decide with the owner.** Each conflict is settled against the code first (which statement the code follows, and the lines that show it) and put to the owner as a question in the spec's own words. Answers become decision records.
6. **Gate changes.** `stele change` checks one spec change against every approved requirement, translating only what the change adds or edits. The owner approves the readings, which are pinned to the text and to the rule; `stele gate` blocks unapproved or conflicting rules.
7. **Learn.** The owner's answers are saved as lessons in the registry, committed with the specs, and every later check follows them.

## Quick start

```sh
curl -fsSL https://raw.githubusercontent.com/provasign/stele/main/install.sh | bash   # or: brew install provasign/tap/stele
cd your-repo && stele init
```

That is all you run. `init` finds your spec format and coding agents, and writes Stele's short instructions and hooks for them; commit what it writes. From then on, Stele works through the agent:

- **When you ask for a change,** the agent gets the approved requirements it touches (Claude Code and Codex), so its plan keeps them or asks you before changing one. Codex runs a project's hooks only after you review them once with `/hooks`; Stele never records that trust for you.
- **When specs change,** the agent runs Stele's check and asks you only what needs a person, in the specs' own words.
- **When you commit,** a pre-commit hook blocks specs and approved requirements that disagree, and says what to run.
- **On a project with specs already,** the first commit that touches them says how to adopt: the agent reads every requirement once and asks you about each contradiction it finds.
- **After an edit,** the agent hears which approved requirements the code change touches, by matching the code's names to each rule's step and attributes. That says where to look; it does not check the code against the rules.
- **After a merge or pull,** run `stele check` now and then. Each change was checked only against what its own branch held, so two changes approved on different branches can contradict each other once they meet. It checks every approved requirement against every other, with no model and in seconds, and names each contradiction in the requirements' words, with who approved each side.
- **On the pull request,** Stele commits a small card (`.stele/cards/<change>.json`): what the change does to the requirements, your decisions, and the approved requirements the code touches. Shale shows it on the PR.

**Without spec documents.** In a repository that uses Shale and has no spec folder, `stele init` takes the requests the agents declare with `shale intent` as the specs. Right after the agent declares one, before it edits, Stele checks it against every request approved before and asks you about any contradiction: change the new request, retire the earlier one, or restate both as one rule with its exception. Retired requests are listed in `.stele/retired-intents.yaml`; Shale's own evidence is never edited.

Stele needs the [Z3](https://github.com/Z3Prover/z3) solver on PATH; the installer tells you the command for your system. To build from source instead: `GOWORK=off go build -o bin/stele ./cmd/stele` (Go 1.26). `stele --help` lists every command, and the [docs](docs/README.md) explain each step.

## What to expect

Measured on the case studies and held-out sets (single runs, small labelled sets):

- **Finding contradictions** works best by review: on Project A's ten known contradictions an LLM review found 9 at high reasoning effort, and on two independently labelled sets it found every labelled pair.
- **The formal path** finds fewer, but each conflict it reports is a solver-checked contradiction among the approved rules: 16 of 20 labelled conflicts on one held-out set, with every finding real, but 4 of 13 on another and 4 of 10 on Project A.
- **Translation** is the weak stage. On a sample of 20 statements from a held-out project, graded blind, rules said exactly what their statement said for 3 before the latest grammar and coverage changes and 5 after; misreadings fell from 2 to 0, and most of the rest are weaker than their statement rather than wrong. A rerun can read a statement differently. That is why the owner approves every reading, with Stele's findings beside it.
- **Settling from the code** was right in every verdict checked by hand.

**Cost.** The hooks never call a model. On two real registries, a request about specified behaviour added about 400 to 600 tokens of approved requirements to the agent's context (at most about 750), an unrelated request usually none, and each lookup took under 25 ms. Model spend comes only from translating specs: adopting a 177-statement project cost $2.28, and `stele adopt` and `stele change` take `--max-cost` to cap it.

**What a check means.** A reported conflict is proved for the rules as approved: they cannot all hold, and Stele shows a situation where they do not. "No conflict" is a statement about those rules, not about the prose, and holds within stated limits: each operation is checked one step from any state the invariants allow, so a conflict can sit in a state no sequence of steps reaches; collections and other instances of an entity are checked over a bounded number (3 by default); a solver timeout is `unknown`, never a pass. Stele does not prove the code implements the spec. [What a verdict means](docs/06-checker.md#what-a-verdict-means) states the semantics.

## Not yet

- Running `stele gate` in GitHub Actions without setting it up yourself.
- Generating tests from the rules, and checking that a code change keeps the rules it touches (`stele touches` finds them by name only).
- Learning rules from the code itself, in projects with neither specs nor Shale.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
