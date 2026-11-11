# 01. Vision

Stele is a registry of intended behavior over time, for any software project in any domain. It holds every requirement as a typed rule over one shared domain model and checks all active rules together on every change, so a spec that contradicts one written six months ago fails like a type error.

## The problem

Spec-driven development accumulates specs. Each spec is reviewed when it is written, against whatever the reviewer remembers. Nothing re-reads last spring's spec when this autumn's lands.

- Prose specs have no semantics a machine can check.
- Vocabulary drifts, so two specs written months apart can both define what "removing a resource" means.
- AI coding agents implement whatever spec they are handed. Given two contradictory specs, they implement both.

The result is contradictions that surface as production incidents, found by customers.

## The thesis

Two specs can only be compared if they speak the same vocabulary.

1. Enforce one vocabulary: every rule references declared symbols only.
2. Store every requirement as a rule in a decidable intermediate representation (IR): the system spec.
3. Treat all active rules as one system and check it like a codebase: on every change, with counterexamples, so conflicting, superseded and updated requirements are found by logic, not memory.
4. Then bind the code and its tests to the system spec, so what was built is checked against what was specified.

AI coding agents write most specs and build from every statement in them. A statement the registry cannot check still steers the code, so every statement must end checked ([D-049](design-decisions.md)). The MVP is steps 1 to 3: keep specs checked. Matching code and driving tests (step 4) are required, but come in Phase 2.

## What is deterministic

Name resolution, type checking, conflict detection, gap detection and invariant checking are deterministic. Two steps are probabilistic, and both are gated:

| Probabilistic step | Gate |
| --- | --- |
| Translating prose into IR | The owner approves each rule, read back in English with concrete examples |
| Grounding prose terms to symbols, and inferring the model from code | Structural duplicate detection, agreement gating, the owner's review of every new symbol and synonym |
| Finding candidate contradictions by reviewing the whole corpus | The solver confirms what it can model; the code settles which statement holds; the owner decides ([D-045](design-decisions.md), [D-055](design-decisions.md)) |

Measured on the case studies, review finds more contradictions than translation and checking alone, and the solver confirms the ones it can express, with a situation in which the rules cannot all hold, so Stele uses both: review discovers, the solver proves, the code settles, the owner decides. Grounding is the single point of failure for cross-time detection: a rule mapped to the wrong or a duplicate symbol is invisible to every later check. Most of the guardrails in [07](07-translation-and-grounding.md) exist for that reason.

## Who it is for

Any software project whose intended behavior is written down, changes over time, and has an owner who decides what it should do: a maintainer, a lead, or a small group acting as one. The core knows no domain: it has contexts, entities, operations and rules, and nothing about money, patients, devices or users.

The same model covers a SaaS backend, an embedded controller, a data pipeline, an infrastructure policy set, a public API, a game server or a CLI tool. [domain-coverage.md](domain-coverage.md) shows how each kind of project maps onto it, and [examples/](examples/README.md) walks through unlike domains end to end.

What varies by project is not whether the registry applies but how much of the behavior it can prove. Every statement is checked by the solver against the others, verified by a measurement (Phase 2), or reviewed by its owner when it is a judgment. A statement that is none of these yet is an obligation and counts as unchecked. A project gets the most value where rules are dense, long-lived and owned by several teams.

## Scope of v1

The proved level covers behavior over finite state, in any domain:

- permissions (who may do what, when),
- preconditions,
- invariants,
- state machines,
- time windows and grandfathering, as bounded integers,
- counts over bounded relations, as abstract bounded integers.

Statements outside that fragment are not dropped. Performance budgets, SLOs and resource limits are `measured` rules: they are recorded and tracked now; their thresholds are to be checked against each other, and against tests or telemetry from Phase 2. Behavior the IR cannot yet express is an `obligation` that names the construct it waits for, and counts as unchecked. UX, ML quality and other judgments are `informal` rules, reviewed by their owner for every text version. See [domain-coverage.md](domain-coverage.md#assurance-levels).

The proved fragment is meant to grow until obligations are rare. Multiple instances with quantifiers, strings and records, and what a step produces are implemented ([D-050](design-decisions.md), [D-064](design-decisions.md)). Still planned: future-time ordering over bounded traces, references to an earlier snapshot, and idempotence of operations ([05](05-ir-and-semantics.md#planned-extensions)).

## Prior art and positioning

| Tool | What it does | How Stele differs |
| --- | --- | --- |
| AWS Kiro Requirements Analysis | LLM-to-logic translation and SMT checking of one spec's requirements, with two-option questions; derives property-based tests | Checks every spec against every approved rule, over time, in any of four formats, and keeps the decisions in git. See [prior art](prior-art.md#spec-tools-for-coding-agents) |
| AWS Cedar | Sound and complete SMT analysis of authorization policies, verified in Lean | Stele is meant to emit Cedar for the permission subset (not built yet) and adds invariants, state machines, prose ingestion and history ([D-012](design-decisions.md)) |
| OPA, OpenFGA | Policy evaluation and relationship-based authorization | Targets for runtime guards, not competitors |
| DMN | Decision tables for business decisions | Decision tables are an authoring form here, lowered into the same checked IR |
| Spec Kit, OpenSpec, Tessl | Spec authoring formats for AI-assisted development; Spec Kit's `analyze` asks a model for inconsistencies within one feature | Authoring front ends; Stele is the shared back end that checks across them |
| NASA FRET, GE ASSERT, SpeAR | Structured or controlled-English requirements compiled to logic; realizability and consistency checks | Exact translation because the author writes in the tool's language; Stele takes the specs teams already write |
| AWS Automated Reasoning checks | LLM extracts variables and rules from a policy document; SMT checks chatbot answers against it | Checks answers against one document; Stele checks the documents against each other |
| VeriMed, SAT-LLM (research) | LLM to SMT, conflict and vacuity checks on 55 to 144 curated requirements | Hand-written or small vocabularies and no precision on unmodified specs; Stele builds the vocabulary from real repositories. See [prior art](prior-art.md) |

The defensible product is not a permission checker. It is the registry: prose in, one checked model of permissions, invariants and state machines, history and decisions kept.

## The honest claim

Stele checks the approved rules, the specification as the owner approved its reading, against each other. A conflict it reports as proved has a witness, a situation in which the rules cannot all hold; a contradiction it lists as possible comes from reading the texts, and is the owner's to judge; the absence of one holds for those rules, one step deep and within stated bounds ([06](06-checker.md#what-a-verdict-means)). How faithfully the rules read the prose is measured, not proved, which is why the owner approves every reading. Deriving tests from the rules is Phase 2. Stele does not prove the code implements the specification. See [09](09-enforcement.md#the-implementation-gap).
