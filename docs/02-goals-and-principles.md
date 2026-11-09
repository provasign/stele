# 02. Goals, non-goals and principles

## Goals

1. Serve any software project in any domain from one domain-free core.
2. Detect any logical conflict between an incoming rule and every active or deprecated rule, regardless of when either was written.
3. Detect gaps: reachable states and operation × role pairs that are declared as needing a rule but have none.
4. Keep the process light: one owner approves, one gate decides ([D-056](design-decisions.md)).
5. Record every conflict resolution as a versioned decision, never as silent shadowing.
6. Bind code and tests to a registry snapshot, and fail CI on drift (Phase 2).
7. Keep authoring cost close to writing prose, with the LLM doing formalization and people confirming it.
8. Check every normative statement. Specs, mostly written by AI agents, are what the system is built from; a statement Stele cannot check is a gap in Stele, not in the spec. Keeping specs checked is the MVP ([D-049](design-decisions.md)).

## Non-goals

- Proving functional correctness of implementation code.
- Proving UX, performance or other non-functional qualities. They are verified (`measured`) or, for judgment only, reviewed (`informal`).
- Shipping domain knowledge in the core.
- Replacing tests. The registry generates and governs tests.
- Undecidable logic. The IR grows to express what specs state, but every construct keeps a decision procedure, bounded where necessary, and bounded results say so.
- Replacing Cedar or OPA as runtime policy engines.

## Design principles

| Principle | Meaning | Where it is enforced |
| --- | --- | --- |
| The LLM proposes, never decides | Every LLM output passes a deterministic check or a human approval before it becomes authoritative | Translator, inference, owner approval |
| One vocabulary | No rule may reference a term not declared in the domain model | Compiler name resolution |
| Domain-neutral core | The core knows types, contexts, entities, operations, actors and rules, and no domain nouns; domains live in packs and plugins | [Neutrality rules](domain-coverage.md#neutrality-rules-for-the-core) |
| Decidable within a stated scope | The IR has the primitive types programs have, each with operations the solver decides ([D-050](design-decisions.md)); collections, instances and traces are checked up to a declared bound; richer theories are opt-in. A check the solver cannot decide is `unknown`, never a pass. Every result names its tier and bound | Grammar ([05](05-ir-and-semantics.md)), [theory tiers](domain-coverage.md#theory-tiers) |
| Nothing goes unchecked | Every normative statement is codified and checked with all others, or reviewed by its owner when it is a judgment. A statement the IR cannot express yet is an obligation naming the missing construct, and counts against coverage. A new or edited statement cannot merge as none of these ([D-049](design-decisions.md)) | [Assurance levels](domain-coverage.md#assurance-levels), the spec gate ([08](08-workflows.md#open-conflicts-and-the-ratchet)) |
| Closed world | Every operation declares a default effect, so absence of a rule is never ambiguous at runtime | Operation schema |
| Explicit coverage | Where a rule is expected is declared, not guessed, so gap detection is deterministic | `coverage` on operations |
| Conflicts are data | Precedence lives in decision records and named policies, each approved by the owner with a rationale | Decision schema, precedence policies |
| Spec as code | Changes arrive as pull requests, are reviewed, versioned and gated in CI | `stele gate` |
| Counterexamples, not verdicts | Every failure comes with a concrete state a person can read | Checker output contract |
| LLM-free core | Parsing, type checking and solver checks run with no API key | Packaging |
| No production data to the LLM | The translator sees prose, symbols, code locations and solver-generated examples, never real records | Translator inputs |
