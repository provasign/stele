# Domain coverage

The core of Stele knows no business domain. It knows types, contexts, entities, operations, actors, rules and decisions. Everything domain-specific (vocabulary, extractors, test emitters, typical rules) lives at the edges as plugins and packs, so the same core serves any software project.

This document describes the design. Built today: the domain-free core, the `informal`, `obligation` and `measured` kinds (measured rules are recorded and tracked, not yet checked), and importers for OpenSpec, Spec Kit, Kiro, Markdown and Shale requests. Domain packs, extractors, test and guard emitters, measurement adapters and theory tiers 1 and 2 are not built.

This document states what "any project" means precisely: every project can register all of its intended behavior, every statement ends checked, and each gets the strongest assurance level its content allows.

## The generic vocabulary

Every concept in the IR is defined structurally, not by domain.

| IR concept | Structural definition | Web service | Embedded device | Data pipeline | Infrastructure | Library or CLI |
| --- | --- | --- | --- | --- | --- | --- |
| Context | A namespace for related vocabulary | Service or feature area | Subsystem (drive, door, sensor bus) | Domain data product | Platform team, account, cluster | Package or module |
| Entity | Anything with state | Record, resource | Device, mode, channel | Dataset, table, job run | Cloud resource, environment | Config, session, file |
| Attribute | A typed piece of entity state | Column, field | Register, sensor reading, flag | Column, classification, region | Tag, size, version | Option, flag value |
| Operation | Anything that changes state | Endpoint, mutation | Command, event, interrupt | Job, transform, export | Plan, apply, deploy | Function, subcommand |
| Actor | Anything that initiates an operation | User role, service | Operator, controller, scheduler | Job owner, service account | Engineer, CI bot | Caller, end user |
| Request | The context of one invocation | HTTP request | Command frame | Job trigger | Change request | Call arguments |
| Rule | A constraint on operations or state | Permission, precondition | Safety interlock | Data contract, access rule | Change policy | API contract, flag compatibility |

The `platform` context is built in and provides only the skeletons of `Actor` and `Request`. Each project declares its own actor kinds, roles and request attributes ([04](04-data-model.md#built-in-actor-and-request-entities)).

## Assurance levels

No statement is rejected for being outside the solver's reach, and none is left unchecked. Each rule declares, or is classified into, one of three levels ([D-030](design-decisions.md), [D-049](design-decisions.md)). Specs are what the system is built from, and AI agents act on every statement, so a statement that is merely filed would still steer the code.

| Level | Rule kinds | How it is assured | Typical content |
| --- | --- | --- | --- |
| **Proved** | `permission`, `precondition`, `invariant`, `derivation` | SMT solver, across all rules, on every change | Who may do what, safety interlocks, state machines, data invariants, flag compatibility |
| **Verified** | `measured`, and any proved rule's generated tests | Recorded and tracked today; planned: bound to a test, benchmark or telemetry query and checked in CI or by monitoring from Phase 2 | Latency budgets, SLOs, memory and power limits, throughput, error rates |
| **Reviewed** | `informal` | Judgment only: an owner reviews it for every text version, and the review is recorded with the snapshot; owned, linked to prose and code, versioned | UX quality, tone, ML model quality, legal judgments |

A statement is `informal` only when its content is a judgment no check can decide. "Deletion must be fast" is not informal: it needs a threshold from the owner, and then it is `measured`.

A statement that fits none of the levels yet is an `obligation`: behavior the IR cannot express. It records its prose and the construct it is waiting for, and counts as **unchecked**. Obligations are the backlog that drives the IR's growth ([D-049](design-decisions.md)); when the construct lands, the obligation is translated again and becomes a proved rule. From Phase 2 an obligation can also bind a test, which checks the code but still not the spec:

```yaml
id: R61
kind: obligation
subject: Server.start
prose: "Startup is byte-identical to the pre-feature sequence."
awaits: snapshot-reference      # one of the D-049 extensions
test: { kind: snapshot, ref: tests/startup_golden_test.go }   # optional, Phase 2
status: active
```

Coverage is reported per level, over all normative statements. Obligations, `needs_vocabulary` and `invalid` statements count as unchecked, against Stele rather than against the spec. The MVP's measure is how many statements are checked.

A `measured` rule:

```yaml
id: R50
kind: measured
subject: Resource.delete
metric: latency.p95
unit: ms
threshold: { max: 300 }
source: { kind: benchmark, ref: bench/delete.k6.js }   # or prometheus, test, trace
status: active
```

Measured rules can still conflict, and the checker is to find those (not built yet): two measured rules on the same subject and metric with incompatible thresholds are a conflict, the same as two permission rules.

## Theory tiers

Every project gets the primitive types programs have: bool, int, decimal, enum, string, duration, time, list, set, map, optional values, records and relations to many ([D-050](design-decisions.md), [05](05-ir-and-semantics.md#types)). The core is what Stele always decides; the tiers are theories still to come, each opted into per context, and every result from an opt-in theory carries its tier ([D-031](design-decisions.md)).

| Tier | Theories | Solver guarantee | Who needs it |
| --- | --- | --- | --- |
| Core | Booleans, enums, linear integer and real arithmetic, durations, times and the clock, strings with equality, prefix, suffix, containment, concatenation and regular expressions, lists, sets and maps, optional values and relations, records, bounded quantifiers over collections and relations to many, operation history and old values | Answers within the budget, or reports `unknown`; results over collections and relations to many hold up to their declared bounds | Every project |
| Tier 1 | Fixed-width bit-vectors (with `&`, `\|`, `<<`) | Decidable; can be slower | Embedded (registers, overflow) |
| Tier 2 | Non-linear arithmetic | May return `unknown` for reasons other than time | Formulas |

Tiers 1 and 2 are not implemented yet; statements needing them are obligations ([D-049](design-decisions.md)).

A context will enable a tier explicitly:

```yaml
context: motor-control
theories: [bitvector, real]
```

Results report their tier, so a `pass` from tier 2 is never presented with the same confidence as a core `pass`.

## Extension points

| Extension | What it adds | Interface | Example plugins |
| --- | --- | --- | --- |
| Domain pack | Contexts, entities, operations, synonyms and typical rules for a domain | A published context, imported by version | Accounts and identity, data privacy, industrial safety, cloud governance |
| Extractor | Draft symbols with anchors, from a source format | Reads files, emits candidate symbols with `anchors` | SQL, OpenAPI, protobuf, GraphQL, ORM, C enums and state tables, Terraform, Kubernetes, dbt, Avro, CLI argument parsers |
| Importer | Rules from another spec format | Emits candidate IR | Built: OpenSpec, Spec Kit, Kiro, Markdown, Shale requests. Planned: EARS, Gherkin, DMN, Rego, Cedar |
| Test emitter | Generated tests in a language and framework | Takes a rule slice and examples, emits tests | Hypothesis, fast-check, jqwik, Go testing, Rust proptest, C Unity, pytest for data checks |
| Guard emitter | Runtime checks | Takes a rule slice, emits code or policy | OPA/Rego, Cedar, C assertions, SQL constraints, Kubernetes admission policies |
| Measurement adapter | Values for `measured` rules | Returns a metric value for a subject | k6, JMH, Criterion, Prometheus, OpenTelemetry |
| Anchor kind | A new kind of code location | Name + resolver | `http`, `table`, `topic`, `register`, `state`, `resource`, `symbol`, `flag` |
| Theory | A new solver theory | Core-maintained only; must lower to SMT-LIB and declare its tier | Bit-vectors, reals, strings |

Theories are the one extension point the core keeps for itself, because a wrong theory makes every proof wrong.

## Neutrality rules for the core

These rules keep domain assumptions out of the core. Reviewers apply them to every core change.

1. No domain nouns in core code, schemas or error messages. Words such as account, user, order, patient or device appear only in packs, examples and tests.
2. Built-in contexts are limited to `platform`, which provides `Actor` and `Request` skeletons with no roles or kinds.
3. Every feature must be demonstrated in at least two of the [examples](examples/README.md) from unlike domains before it is accepted.
4. Anchors, extractors, emitters and adapters are plugins. The core ships reference plugins but treats them like any third-party plugin.
5. Default policies (precedence, timeouts, coverage) are domain-free and configurable per context.

## What does not fit, and what happens

| Behavior | Why it is not proved | What the registry does |
| --- | --- | --- |
| Performance, latency, resource use | Depends on hardware and load, not logic | `measured` rule bound to a benchmark or telemetry |
| Ordering of operations ("unload before reload", bounded retries, every five minutes) | Needs history or time | Proved: history attributes, `old` and the clock ([D-051](design-decisions.md)); liveness without a deadline is planned as a bounded-trace check |
| Concurrency (races, deadlocks between threads) | Needs temporal logic over interleavings | `obligation`, with a stress or model-checking test from Phase 2; state-machine checks cover declared modes; integration with TLA+ or model checkers is a future importer |
| "Unchanged from before" | Needs the earlier behavior | Planned: references to an earlier snapshot, which a registry that holds the earlier spec can supply; until then an `obligation` |
| Continuous dynamics (control loops, physics) | Needs differential equations | Interlocks and mode logic are proved; the control law itself is `measured` |
| ML model behavior | Not a logical rule | Access, data and fallback rules are proved; quality is `measured` |
| UX and visual design | Judgment | `informal`, reviewed per text version; UI flows as state machines are proved |
| Rules over unbounded data (all strings, all graphs) | Undecidable in general | Bounded models labelled with their bound, tier-2 theories, or derived attributes |

## Coverage of software types

| Project type | What is proved | What is verified or tracked | See |
| --- | --- | --- | --- |
| Web service or SaaS backend | Permissions, lifecycle state machines, data invariants, cross-feature conflicts | Latency SLOs, UX | [examples/web-service.md](examples/web-service.md) |
| Embedded, IoT, robotics | Safety interlocks, mode machines, command preconditions | Timing budgets, control quality | [examples/embedded-controller.md](examples/embedded-controller.md) |
| Data pipeline or warehouse | Data contracts, access and residency rules, retention, lineage constraints | Freshness, row-count SLOs | [examples/data-pipeline.md](examples/data-pipeline.md) |
| Infrastructure and platform | Change policies, freeze windows, environment promotion rules | Cost budgets | [examples/infrastructure-policy.md](examples/infrastructure-policy.md) |
| Library, SDK or CLI tool | API contracts, option compatibility, deprecation and versioning rules | Performance, ergonomics | [examples/cli-tool.md](examples/cli-tool.md) |
| Mobile and desktop apps | Navigation and onboarding flows as state machines, permission prompts, offline rules | Rendering performance, UX | — |
| Games | Economy invariants, matchmaking rules, progression state machines | Balance, fun | — |
| Regulated systems (health, finance, aviation) | The above, with `final` rules for non-overridable obligations and the audit trail | Certification evidence beyond logic | — |
