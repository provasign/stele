# 09. Enforcement

Enforcement is Phase 2 ([D-049](design-decisions.md)). The MVP keeps the specs themselves checked; this document describes what follows.

Enforcement binds the approved rules to running code through a code check on every PR, generated tests, optional runtime guards, traceability links and the CI gate. It narrows the gap between specification and implementation; it does not close it. One owner and one gate apply here as everywhere ([D-056](design-decisions.md)).

## Code check on every PR

The first enforcement step reuses the code verdicts of adoption ([D-055](design-decisions.md)). Each verdict cites the lines that show which statement the code follows. A code PR that changes a file holding such evidence re-runs the verdict for those rules ([D-057](design-decisions.md), proposed). If the code now does otherwise, the PR is reported: either the code regressed or the spec needs a change. This needs no per-language adapter, so it comes before test generation.

## Test generation

- One property-based test suite per rule, in the project's language: Hypothesis (Python), fast-check (TypeScript), jqwik (Java).
- Each suite is seeded with the solver's counterexamples and approved examples, including boundary cases (`activeHolds = 0` and `= 1` for R12).
- Tests drive the real handler through an adapter the engineer writes once per operation, mapping registry state to fixtures.
- Generated tests are named `T-<rule>-<n>` and regenerate whenever the rule or its slice changes.

### Conformance tests for state machines

For each entity with a state machine, a generated conformance suite drives the real system through declared transitions and checks that:

- every declared transition is possible through its operation,
- no undeclared transition is observed,
- terminal states have no outbound transition.

## Runtime guards

Optional generated policy checks for critical rules, emitted as code, OPA/Rego or Cedar policies.

A guard returns a decision plus an explanation ([D-027](design-decisions.md)):

```json
{
  "decision": "deny",
  "rules": ["R12@3"],
  "decisionRecord": null,
  "explanation": "Denied by R12: resource has 2 active holds.",
  "snapshot": "sha256:4be1..."
}
```

Each rule declares `explanation` visibility:

| Value | Shown to end users | Shown to support and logs |
| --- | --- | --- |
| `user` | Template rendering | Full |
| `internal` | Generic message | Full |
| `redacted` | Generic message | Rule ID only (fraud and abuse rules) |

## Traceability

- Code is linked to rules with annotations such as `@implements R12`.
- CI checks every annotation against the pinned snapshot. A reference to an unknown or retired rule fails.
- The coverage matrix shows active rules with no implementing code and no passing test.

## CI gate

`stele gate` is the one merge condition, for spec and code changes alike. CI fails when any of these holds:

| Condition |
| --- |
| Checker error, or `unknown` in the slice |
| The change adds a conflict that is not an acknowledged open conflict |
| A rule the change touches is not approved, or its statement changed after approval |
| A vocabulary change alters what an approved rule means (a value's range or values, what a step writes) and the rule's approval does not name it |
| A rule's code verdict changes from followed to contradicted (once the code check is built) |
| An active rule has no linked passing test (once tests are generated) |
| An `@implements` reference does not resolve |

There is no fast path, waiver or lock file ([D-056](design-decisions.md)). A refactor that changes no behaviour passes because the code verdicts and tests still hold. When code must ship ahead of its spec, the code check reports the difference on every run until the owner updates the spec or the code.

## The implementation gap

The registry proves consistency of intent. It does not prove the code implements it. Three mitigations, none complete:

1. Property tests seeded by solver counterexamples, which catch the cases most likely to be wrong.
2. Runtime guards for critical rules, which move the rule into the execution path.
3. Conformance tests that compare observed state transitions with declared ones.

Behavior outside the modeled rules is not covered by any of them; an obligation's bound test covers the code, not the spec. Documentation, release notes and sales material must never claim "verified implementation". The honest claim is "consistent specification, with tests derived from it."
