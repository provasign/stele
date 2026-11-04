# 06. Checker

The checker is the deterministic core. Every check returns a verdict plus either a counterexample state or an unsat core, and every timeout is reported as `unknown`, never as a pass.

## Checks

| Check | Question asked | Engine | Severity | Status |
| --- | --- | --- | --- | --- |
| Response conflict | Is there a situation for which no response satisfies every active rule after decisions? Includes permission conflicts: an allow and a deny that both remain after decisions, final rules and in-context precedence ([05](05-ir-and-semantics.md#situation-and-response)) | SMT (Z3; Cedar for the permission subset from Phase 2) | Error | Built (Z3); final rules and in-context precedence planned |
| Final-rule clash | Can two `final` rules with opposite effects both match? | SMT | Error | Planned |
| Invariant preservation | Can any operation, under its effects and conditional effects, take a state satisfying all invariants to one that violates one? | SMT | Error | Built, one step deep |
| Gap | For each role in `coverage.required`, is there a reachable state where no explicit rule matches? | SMT enumeration per operation × role | Error | Planned |
| Vacuity | Does a rule never match, or never change the outcome? Also: can an invariant hold only if its situation never arises, because other rules forbid what it requires there ("warn when the deadline is near" against "never show a warning")? The state check passes such a pair, since the system may keep out of a situation it controls | SMT | Warning | Built |
| Dead response | Is there a system-controlled value, or an operation, that no situation allows? Rules that make a state unreachable conflict as surely as rules with no common response (translator experiment (stele-research `research/translator/README.md`)). A default-deny operation no rule allows is reported apart, as never allowed: a vocabulary choice, not a contradiction | SMT, one query per value; unsat core minimized by deletion | Error | Built |
| Reachability | Can each non-initial state be entered? | Bounded model checking | Error | Partial: conflicts in states no `createdBy` step can enter are set apart; bounded model checking planned |
| Deadlock | Can each non-terminal state be left? | Bounded model checking | Warning | Planned |
| Shared write | Do two contexts write the same attribute? | Graph query | Error | Planned |
| Stale decision | Does a decision resolve a rule version that is no longer current? | Graph query | Error | Built (the loader rejects a decision on an old rule version) |
| Table overlap and completeness | Do two rows of a `unique` table both match? Is any input combination unmatched? | SMT | Error / warning | Planned |
| Translation ambiguity | Are N sampled formalizations equivalent? | SMT equivalence | Question to author | Built (`translate --drafts` above 1) |

Planned: conflicts with a `deprecated` rule become warnings, listing the deprecated rule's dependants ([D-005](design-decisions.md)). Today every conflict is an error.

The Phase 2 and Phase 3 parts below (Cedar, the cvc5 portfolio, the nightly full run and the solver-disagreement freeze) are not built; each section says so.

### Gap detection

Under a closed world, every operation has a defined outcome, so "a rule was expected" must be declared. An operation's `coverage.required` lists the roles that need at least one explicit rule. The checker reports a gap for role `x` when a reachable state exists where no explicit rule for `x` matches and only the default applies ([D-015](design-decisions.md)).

Operations without `coverage` declarations are not gap-checked. The coverage matrix in the UI shows which operations have no declaration.

## Conflict enumeration

The checker reports every minimal conflicting set of rules, not only the first ([D-039](design-decisions.md)). It asks once for the state and once per operation ([D-042](design-decisions.md)):

1. Give each rule a selector. Ask the solver for a situation and a selection such that no response satisfies the selected rules, where no earlier core is wholly selected. "No response" quantifies over the response, and three ways of deciding it race. The first definite answer wins, and the search is `unknown` only when all three are:
   - **Refinement over concrete responses.** Ask for a situation and a selection that every response collected so far fails. Then ask for a response that satisfies the selection in that situation. If there is none, the situation is the answer. If there is one, collect it and ask again. If no situation is left, there is none. Responses are collected as generally as the rules allow, so one can answer many situations and selections:
     - A response value that equals a term over the situation that a rule or the frame sets it to (`v' = t`) is collected as that term, for example "unchanged" or "named after the VM".
     - So is a whole number at the edge a linear bound in a rule allows it (`floor` or `ceil` of the bound), as in "the whole days overdue" stated by two bounds, and a string a rule says the value contains (`contains`, `startsWith`, `endsWith`), alone or all of them joined, as in "the message names the overdue tool". A map is collected as its entries, `store`d where the rule gives each (the result per tool, for the tools present), and the positions of a list prefer different terms, so one response lists every item. Whether a value equals a term is asked of the solver, since two equal maps can print differently, and a constant is never a term. A term that names another response value is resolved through that value's own terms. Without these, each response was a constant answering one situation, and the loop never converged.
     - The response query prefers, as soft constraints only, a response that also meets the rules not selected, that keeps values, and that equals one of these terms.

     The queries are quantifier-free, and the loop gives up after 32 responses.
   - **The quantified query under quantifier elimination,** which decides linear integer arithmetic completely, if slowly.
   - **The quantified query under z3's default engine.**

   Each way is exact. Each is fast on queries where another can spend the whole timeout: refinement on Boolean frames, the default engine on most satisfiable queries, and elimination on unsatisfiable queries over integers. A `sat` whose model the solver did not finish giving counts as `unknown`.
2. Fix that situation and shrink the selection by deletion until removing any rule makes it satisfiable. That is the core.
3. Block the core and repeat until the solver finds no situation. A solver `unknown` stops the enumeration with verdict `unknown`.
4. Drop any core that strictly contains another. A core is minimal in the situation where it was found, but a smaller core found later, in another situation, makes it non-minimal. Every minimal set is found: the search ends only when every remaining selection contains a found core.

Because the search covers the whole slice, it finds conflicts that span groups of rules a person would check separately. On the Project A case study it found two that the hand-made slices missed.

The implementation bounds enumeration at 100 new conflicts per unit. It checks whether another core remains before treating the search as complete; unfinished enumeration adds the unit to `undecided`. Existing failures still produce `fail`, and tolerating all reported failures then leaves `unknown`, which blocks the gate. Current, version-pinned open conflicts do not consume the gate's limit, but they are still solved and reported before the ratchet filters them. Closed or stale records receive no exemption. A baseline refuses an incomplete check.

## Output contract

```json
{
  "check": "permission-conflict",
  "verdict": "fail",
  "rules": ["R12@3", "R31@1"],
  "contexts": ["resources", "privacy"],
  "counterexample": {
    "actor": { "role": "User", "isOwner": true },
    "request": { "kind": "erasure" },
    "self": { "status": "active", "activeHolds": 1 }
  },
  "english": "An owner making an erasure request on a resource with 1 active hold is denied by R12 and allowed by R31.",
  "sliceHash": "sha256:9f2c...",
  "solver": { "name": "z3", "version": "4.13.x", "ms": 412 }
}
```

`verdict` is one of `pass`, `fail` or `unknown`. An unsat core replaces `counterexample` when the failure is joint inconsistency (T-A6).

## What a verdict means

This section states, for readers from formal methods, what is decided and what is trusted.

**The question.** The registry is a transition system ([05](05-ir-and-semantics.md#situation-and-response)). For the state, and for each operation, the checker decides

  ∃ situation. ∀ response. ¬(rule₁ ∧ … ∧ ruleₙ)

For the state, the situation is the environment and the response is the system-controlled attributes. For an operation, the situation is a state satisfying the invariants plus a request, and the response is whether the step runs, what it writes and what it produces; attributes it does not write keep their value. This is a one-step realizability question, not satisfiability of the conjunction. Rules can hold together in some state and still conflict, because the system cannot answer some situation it does not control. A `possible` permission ("MUST be able to") is a may-requirement: it asks for one situation in which the step can run with every other rule.

**A conflict is proved.** It comes with a situation and a minimal set of rules that no response satisfies there, and every minimal set is enumerated (up to 100 per unit). It is proved for the rules as approved. Whether those rules say what the prose says is measured, not proved, and the owner approves every reading.

**"No conflict" is bounded.**
- *One step deep.* A step may start from any state the invariants allow, which over-approximates the reachable states. For must-rules that makes a pass sound for every reachable state, while a conflict may sit in a state no trace reaches; conflicts in states no `createdBy` step can enter are set apart. A `possible` rule's witness is likewise a state the invariants allow, not necessarily a reachable one. Multi-step reachability is planned.
- *Bounded instances.* Collections, relations to many and `instances(E)` are checked over a bounded number of other instances: 3 by default, raised to fit a count a rule compares, up to 64. The check's warnings note each raised relation bound and any count beyond 64. Lists default to 3 elements. Within those bounds the encoding is exact; results over them hold up to the bound, in the sense of Alloy's small-scope checking.
- *Exact arithmetic.* Integers, durations and times are mathematical integers, and decimals are exact reals, constrained only by declared ranges. Arithmetic is linear: multiplication by a constant only.
- *Unknown is not a pass.* A query the solver does not settle within its budget is `unknown`, which blocks the gate unless the owner downgrades it for a named slice.

**Policies that change the question.** Two are deliberate, and both are reported:
- Assumptions that contradict each other are reported in their own unit, and every other unit is checked without them. Otherwise no situation would exist and every other conflict would pass unseen.
- An operation's unit assumes only the invariants connected to its rules through shared attributes, so an unrelated contradiction among invariants does not make every step vacuous.

**What is trusted, and how it is tested.** Z3 is trusted, and so is Stele's lowering from the IR to SMT-LIB; neither produces proof certificates. The lowering is tested against an independent brute-force interpreter (`internal/oracle`). The interpreter enumerates every state, request and outcome of small registries and shares nothing with the lowering except name resolution. It never slices, so it also tests the slicing. The checker and the interpreter must agree on every case of the type and grammar suite, and on the conflicts and dead responses of every step of randomly generated spec histories. The state unit's split into independent parts is tested against the whole-unit check. This is testing, not a proof of the lowering.

## Slicing

A change re-checks only the rules it can affect. The slice must be a closure, not one hop ([D-018](design-decisions.md)).

**Status: built for the state unit only.** The checker splits into units (the state, and one per operation). On the measured registries, an operation's rules do not split by shared variables, because every rule there mentions whether the operation runs, so steps are checked whole. The state unit is checked as its parts: the groups of rules, assumptions and variables that share no variable with another group. Each part is cached on its own, so a changed invariant re-solves only its part.

This is sound because the state unit has no frame and no operation default, so its question is its rules, assumptions and variable ranges alone. Parts that share no variable are independent:
- A situation in which no response satisfies the rules exists exactly when one exists in some part, and each minimal conflict lies within one part.
- A rule's condition, or a response value, is ruled out exactly when its own part rules it out, provided every part can hold on its own.

So each part is first checked for being satisfiable with its assumptions, ranges and rules. If one cannot hold, or the solver cannot tell, it would rule out every situation and value of the whole unit, and the state unit is checked whole. Every variable belongs to exactly one part, so an empty range is still seen.

The one visible difference: a state conflict's `situation` lists its part's variables, not the whole state.

The slice for a change is the smallest set S such that:

1. S contains every changed rule, decision and table row.
2. S contains every active or deprecated rule that references a symbol referenced by any rule in S.
3. For every derived attribute referenced in S, S contains the rules and relations it is derived from.
4. For every attribute written by an operation in S, S contains every invariant over that attribute.
5. For every decision touching a rule in S, S contains the decision and both its rules.

Steps 2 to 5 repeat until S stops growing. Permission checks then split per operation and run in parallel. As built, the state unit is checked first, and the steps are then checked concurrently, on up to half the CPUs.

Planned: a nightly full check with no slicing, compared with the incremental runs of the day, where any difference is a slicing bug. Today the slicing is tested against the unsliced check and a brute-force reference checker instead (see [What a verdict means](#what-a-verdict-means)).

## Caching

Results are keyed by the hash of the slice: normalized IR of every member, the solver name and version, and the checker version. Unchanged slices are never re-solved.

**Status: built per unit.** The checker caches each unit's result, keyed on everything the unit asks the solver: the cache format, the checker build (the commit for a clean build, otherwise the executable's size and modification time), the solver version, the limit, the unit's datatypes, variables, formulas and fixed constraints, and the known conflicts. The `stele` binary persists results one file per key in the user cache directory (`STELE_CACHE_DIR` overrides it; `STELE_CACHE=off` keeps none), and the gate, `stele check` and `stele change` reuse them. Because a key names the whole question, a result found on another branch answers the same question on this one. An unreadable entry is a miss. A change re-solves only the units whose question it changes. A new state conflict changes every step's question, because the steps are then checked without the conflicting invariants.

## Timeouts and solver portfolio

- Each solver query has a hard budget, default 10 s locally and in CI.
- The `stele` binary keeps z3 processes running and sends each query after `(reset)`, which clears every declaration and assertion, so each answer is the one a new process gives. z3's own timeout bounds each check. A z3 that does not answer within the budget plus 2 s, or whose answer is no longer needed (another way of deciding won the race), is killed, and its answer is `unknown`.
- A timeout is `unknown`. By default `unknown` blocks merge (T-F4); the owner can downgrade it to a warning for a named slice, which is recorded.
- Planned for Phase 3: Z3 and cvc5 race on hard queries; the first answer wins. In the nightly full run both must agree. A disagreement raises an incident and freezes that slice: merges touching it are blocked until resolved.

## Cedar for the permission subset

Cedar already provides sound and complete analysis of authorization policies. From Phase 2, the compiler emits the permission subset of each slice as Cedar policies and runs Cedar's analysis alongside Z3 ([D-012](design-decisions.md)):

- If they agree, Cedar's result is reported, with Z3's counterexample for readability.
- If they disagree, the slice is frozen as in a solver disagreement.
- Invariants, state machines, preconditions, aggregates and time windows stay in Z3.

Not built: v0 ships Z3 only, because invariants and state machines need it anyway.
