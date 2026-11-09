# 07. Translation, inference and grounding

People confirm the model; they do not author it. Prose is translated into candidate rules over one vocabulary proposed for the whole corpus, Stele checks its own reading, and the owner approves it. The owner's answers become lessons that every later translation reads.

Grounding, the step that maps a prose term to a symbol, is the single point of failure for cross-time detection. Everything in this document either reduces its cost or bounds its error.

## Translator pipeline

The translator turns prose into candidate rules and never writes to the approved registry directly. By default its calls run on Claude Code, the agent Stele is run from, with the model that agent is using; `translate.llm` in `stele.yaml` can name the claude CLI, the Anthropic API or any command that reads a prompt on stdin instead, and `translate.model` pins a model. Every backend gets the same prompts.

1. **Segment.** The importer extracts normative statements ("must", "cannot", "only if"), each with a stable ID and its source. Every normative statement must end checked ([D-049](design-decisions.md)).
2. **Ground.** Terms map to the vocabulary: the approved registry's, plus facts proposed for the whole corpus first, so one fact gets one name. A new term that may name an existing fact is compared with it, folded in when they are the same, and put to the owner when Stele cannot tell.
3. **Generate.** The model writes rules in Stele's IR against a schema; rules that do not compile go back with the error, and the statement is repaired, not dropped.
4. **Draft.** `translate.drafts` readings per statement, 1 by default. Above 1, the solver compares them, and readings that differ come back to the owner as a question with the state that tells them apart.
5. **Check the reading.** A judge asks which clause of each statement its rules cover and which rules go beyond it, and the solver finds statements whose rules contradict each other or block a step. Each finding is read once more with the finding named; what remains is shown to the owner beside the reading.
6. **Approve.** The owner approves the reading in the coding agent, with the check's summary and its findings; approval pins the rule to the statement's text and to the reading.

### What the translator sees

The statements, the vocabulary, the owner's lessons that concern each statement, and the code's structure (the names of types, functions and fields, read with Grove) ([D-014](design-decisions.md), [D-053](design-decisions.md)). It never sees production records. Examples shown to the owner are situations the solver builds from the rules.

## Building the vocabulary

### Formalizing a corpus from prose

Most projects that adopt Stele have specs and no schema to infer from. `stele formalize` builds the whole registry from the statements alone, with one model and no setup for the project:

```sh
stele import speckit|openspec|markdown <dir> --out statements.yaml
stele formalize --statements statements.yaml --out registry/
stele check registry/
```

1. **Propose vocabulary.** Facts first: across all statements, one attribute per fact, each with a meaning and who sets it (`setBy: world` for time, users and other services, which no operation may write). Then operations and relations, batch by batch, each batch seeing the vocabulary so far. Facts named twice are merged.
2. **Translate every statement** against that vocabulary.
3. **Close gaps.** Parts the translator could not express come back with the terms it was missing. The vocabulary step maps each term to an existing symbol (the mapping goes to the translator as a note) or proposes the symbol it needs, and those statements are translated again. This repeats up to `--rounds` times, and stops early when a round adds nothing or closes nothing.
4. **Infer writes.** A rule about the state after an operation constrains what the operation changes, so the operation writes it, unless the world sets it.
5. **Prune** symbols no rule uses, so unused operations do not show up as dead responses and the owner reviews only what is used.
6. **Check clause coverage and scope.** A rule can compile, contradict nothing and still say less or more than its statement: a condition dropped, "in the exact order" read as "applied", a named message read as "non-empty". Such a rule hides the conflicts its statement should raise, and of 59 translated statements two graders checked by hand, both called only 14 faithful, nearly always for a dropped clause. So after translation the model is asked, per batch, to split each statement into its clauses (each requirement, condition, exception, quantity, ordering or named content) and to say for each whether some rule fails whenever the system breaks it, judging by the rules' conditions and assertions and the meaning of the facts they read. A statement with an uncovered clause is translated once more with the clause quoted; what stays uncovered leaves the statement `partial` with the clause in its reason. `--cover=false` skips the check; the report records every clause and its status.
Before step 1, each statement gets the owner's lessons that concern it as notes ([D-063](design-decisions.md)), from `--lessons DIR` or the `--base` registry; the coverage judge (step 6) follows the reading and scope lessons first.

7. **Check each statement against itself.** The registry is checked once with the solver. A conflict between two or more rules that all come from one statement, with no world fact, is a misreading, not a disagreement between requirements: on the Project A window one statement's rules clashed 67 times and buried every real conflict. Such a statement is translated once more with the clash named and checked again; if its rules still contradict each other they are held out of the registry and the statement ends `invalid` with the clash as its reason, so it never reaches the owner as a conflict. One rule that alone leaves a step no valid outcome blocks the step. That can be what the text means, but it is usually written wrong: a rule on a step states what holds after it, so one that asserts only what the step does not set (a fact the world sets, the request) can only forbid the step, and is either a precondition in disguise or a system output read as an input. On the Project A window one such rule, applied to every step, made 67 of the check's 121 conflicts. Such a statement is read again with that diagnosis, in the same request as a contradicting one; a rule that still blocks is kept and listed in `coverage.md` for the owner. `--self-check=false` skips both.
8. **Report.** Every statement ends in one state: `translated`, `partial` (rules, plus parts the IR cannot express or clauses no rule checks), `informal`, `needs_vocabulary` (a question for the owner, with the missing terms) or `invalid`. Only `translated` and `informal` statements are checked. The parts left over by `partial` and `needs_vocabulary` must become rules or `measured` rules, or be recorded as obligations naming the missing construct, before merge. The report counts every unchecked statement, so the gaps measure the IR rather than the spec ([D-049](design-decisions.md)). `coverage.md` lists every statement not fully translated and why. The vocabulary's health is how many attributes the rules of two or more statements read, and how many statements share an attribute with a statement from another source: statements can only conflict on a shared attribute.

With `--one-pass`, steps 1 and 2 run together: each request proposes vocabulary and rules for up to 100 statements (override with `--batch`), using the vocabulary accepted from earlier chunks. Every requested statement must have an outcome, every rule must compile, and unknown statement IDs or malformed replies go back for repair. If repairs are exhausted, the command fails and the unfinished chunk can be retried.

Each stage saves its state in the output directory; one-pass runs also save every accepted chunk and the usage of each returned reply. Re-running the same command resumes without repeating accepted chunks. Checkpoints are replaced atomically and bound to the ordered statements, seed vocabulary contents, context and translation mode. Changed inputs require a new output directory. Older checkpoints without an input fingerprint also require a new directory; their existing registries remain readable by `stele check`.

### Compact translation

`stele change` (and so `stele adopt`, which runs as a change) always translates this way, over the registry's vocabulary plus the terms the change proposes. For `stele formalize`, `--compact` is an alternative to `--one-pass` that reduces generated output and repair work. It proposes vocabulary only alongside the rules needing it, without the separate facts/vocabulary passes. It keeps the shared vocabulary across documents: independent per-document ontologies could hide cross-document contradictions.

Proposing vocabulary batch by batch gives one fact several names: on the Project A window the idle warning became three attributes on two entities, and the check found 0 of 10 known conflicts where the facts-first path found 3 (and 6 with an Opus vocabulary); on the held-out NLI-RE Library set facts-first found 17 of 20 labelled conflicts against compact's 7. `--compact --facts-first` first proposes the facts over the whole corpus, as the facts-first path does, and every compact request then sees and reuses them; requests still propose what the seed lacks, and pruning drops seed facts no rule uses.

- Requests contain at most 20 statements and 12,000 source bytes by default, stay within a source document, and preserve order. `--batch` and `--batch-bytes` override these limits; an oversized single statement stays intact. These are bounds on source material, not the full prompt or model tokens.
- Requests run in waves of up to `--parallel` (default 4). Each wave packs every pending statement, repairs first, into document-bounded requests sent at once against the vocabulary as it stood when the wave began. Replies are merged in input order, so results do not depend on which call returns first. Each reply's vocabulary is folded in member by member: an entity or operation declared again gains the reply's new attributes, relations and writes; an enum's values and an int's range are widened to cover both partial views; writes of world-set attributes are dropped, as the compiler requires. A member defined differently in kind (type, controller, relation target, operation default) keeps the first definition; only statements whose rules then fail to compile are repaired in the next wave, told which definitions stood. Widening is a modeling choice: it treats two replies' views of one named attribute as one fact, so the coverage and consolidation reports remain the place to catch a wrong merge. Parallel requests see less of each other's vocabulary than sequential ones, so more duplicate names are left for consolidation; consolidation's pair reviews run in parallel too.
- Each rule kind has its own output fields, without empty irrelevant fields or generated rule explanations. Short request IDs replace full source anchors. The compiler restores IDs, original prose and source metadata locally. Informal and unsupported parts retain explanations and coverage entries.
- All entries for a statement must compile before that statement is accepted. Accepted statements and their vocabulary are checkpointed even if their neighbours fail. Repairs include only unresolved statements and their diagnostics. An invalid vocabulary or malformed response still requires retrying the unresolved group; nothing dependent on that invalid vocabulary is accepted.
- Statements still invalid after the repair budget are deferred, excluded from emitted rules, and listed as `invalid` with their diagnostic in `coverage.md` and the report's `deferred` map. Later documents continue. Deferred statements survive interruption and are not implicitly retried on resume. A completed formalization command means the corpus was processed; it does not mean every statement was translated. A request that fails on its own (its `--request-timeout`, a crashed CLI) is a failed attempt for its statements: they are retried in the next wave and deferred after the repair budget, and the run continues. Cancellation, the run's `--timeout` and an exhausted call budget stop the run.
- The compiler handles `=` and `!=` between enum attributes with exactly matching value-name sets by deterministic value-wise lowering. It rejects different domains rather than attempting to align their meanings. This applies to every translation path and avoids model repairs for supported finite comparisons.
- Exact repeated entity/extension/operation definitions from the accumulated proposal are handled locally. Changed definitions and merely similar names are not treated as equivalent. Shared-symbol consolidation still runs. In compact mode, proposed identity aliases with matching types, domains and control are applied by resolved-path substitution and a full compile check. Merges needing a value/controller mapping stay separate and become review flags; accepted statements are not translated again for these proposals. `--no-consolidate` disables that phase at the cost of leaving potential synonyms unresolved. Other formalization modes retain the earlier consolidation/retranslation path.
- Remaining `needs_vocabulary` parts are reported rather than sent through the separate gap-closing loop: compact requests already ask for all expressible missing vocabulary. These remain gaps, not successful translations.

Before consolidation starts, the run writes the registry, `coverage.md` and `formalize.json` (with `consolidated: false`), so a run stopped by its budget or deadline during consolidation still leaves its translation. `--effort` sets the reasoning effort for the Claude Code (`--effort`) and Codex (`model_reasoning_effort`) CLIs as well as the Anthropic API, and is shown in the backend name. `stele bench` scores saved runs on the frozen sets in `research/bench/sets.yaml`: statements checked, known conflicts found, and time with calls in flight.

Every formalization mode now writes `requests.jsonl`, with a durable start and finish record for each backend invocation, elapsed time, prompt/schema/reply byte counts, errors and available token usage. The finish record links to `requests/NNNNNN.json`, which preserves the exact request and raw reply (including malformed replies) for offline diagnosis and replay without another model call. These files contain the supplied specification text and vocabulary. A start without a finish is an interrupted attempt; its remote outcome is unknown. Counts cover journaled invocations, not hidden SDK/provider retries or calls made before journaling was introduced. `--max-calls` limits total recorded attempts in the output directory, including failed attempts and earlier resumes. `--request-timeout` limits each call (compact CLI default: 3 minutes). On Unix, cancellation kills the CLI process group and bounds output-pipe draining. Other platforms bound pipe draining but do not yet terminate descendant process trees.

For a deliberately bounded future trial, use a **fresh** output directory:

```sh
stele formalize --statements sample.yaml --out compact-trial/ --compact \
  --batch 20 --max-calls 4 --request-timeout 3m --timeout 10m
```

The saved Project C checkpoint's 146 entries re-encode from 76,627 to 38,118 rule-payload bytes (50.3% less), preserving their logical fields. This offline measurement excludes vocabulary and is not a latency, token-cost or accuracy result. Fixture tests cover partial repair/resume, provenance, budgets, unsupported parts, and a cross-document Z3 conflict. The first bounded live comparison (2026-10-25) failed to complete either path: compact saved eight of twenty statements while one-pass timed out without an accepted chunk. Distinct-enum equality repairs prevented compact from reaching the second document. Completed-workflow speedup and retained conflict recall remain unestablished. Resuming unchanged input is supported; reuse across edited specs still needs dependency-aware invalidation before it can safely avoid retranslation.

The prompts describe the method in general terms with an invented example domain; no test project's vocabulary appears in them.

### Lazy modeling

Only what a check needs is translated. A change translates the statements it adds or edits; adoption translates the statements a review names as candidate conflicts, those sharing their terms, and areas the owner selects. The rest stays unmodeled, blocks nothing, and is recorded as adoption debt ([08](08-workflows.md#w5-adopting-an-existing-spec-corpus)).

## Grounding guardrails

| Guardrail | What it does |
| --- | --- |
| One vocabulary | A rule may name only declared symbols; the loader rejects anything else |
| Facts first | Vocabulary is proposed across all statements before any is translated, so one fact gets one name |
| Same-term question | A new term that reads like an existing one is compared with it by the model; same terms are folded in, unclear ones become an owner question, and the answer is a lesson |
| Owner approval | New terms are approved with the rules that use them |
| Lessons | What the owner says about the project's words ("Customer and Account are the same person") is read with every later translation |

## Designed, not built

These parts of the original design are not in Stele. They are kept here because the design decisions cite them.

### Model inference

Deterministic extractors would propose vocabulary from artifacts a project already has: database schemas and migrations, API definitions (OpenAPI, GraphQL, protobuf), ORM models and state enums, policy files, event schemas, and UI text. Today vocabulary comes from the statements, and the code's structure is read with Grove.

### Auto-accept

A term mapping would be accepted with no question when every sampled translation picks the same symbol, the score margin is clear, and the structural match is exact ([D-028](design-decisions.md)). Today a same-term question goes to the owner whenever the model cannot tell.

### Also not built

- Code anchors on symbols (an endpoint, a table, a UI control), with renames in code proposed as registry renames.
- A reviewed synonym index with negative examples.
- Domain packs: published, versioned contexts with typical vocabulary and rules for one domain.
- Weekly sampling of recent groundings by a second model, and the mapping metrics that would track them.
