# Glossary

| Term | Meaning |
| --- | --- |
| Anchor | A link from a symbol to its source: a table, endpoint, enum, handler or UI control |
| Assurance level | How strongly a statement is checked: proved (solver), verified (tests or telemetry, Phase 2) or reviewed (owner review of a judgment, per text version) |
| Auto-accept | A mapping resolved with no author question, because samples agree, the margin is clear and the structural match is exact; still approved by the owner with the change |
| Conflict | A situation for which no response satisfies every active rule after decisions and precedence: an allow and a deny that both match, an allow a precondition blocks, or obligations that cannot all be met |
| Context | A namespace grouping related entities, operations and rules; scopes in-context precedence. Carries no ownership ([D-056](design-decisions.md)) |
| Counterexample | A concrete state, produced by the solver, showing why a check failed |
| Coverage | The roles of an operation that must have at least one explicit rule; the basis of gap detection |
| Decision table | A tabular authoring form for dense rules, lowered to one rule per row |
| Decision | A versioned record stating which of two pinned rule versions wins, under which condition, and why |
| Derived attribute | An attribute defined by an expression over other attributes or relations |
| Domain pack | A versioned, published context with vocabulary and typical rules for one domain; never part of the core |
| Final rule | A rule no decision or precedence policy may override |
| Gate | `stele gate` in CI, the one merge condition for spec and code changes: no new conflict, every touched rule approved and current, no code verdict turned to contradicted |
| Governed code | Code containing an anchored handler, an exported operation's implementation, or an `@implements` annotation |
| Grounding | Mapping a prose term to a registry symbol |
| IR | The intermediate representation: the typed, decidable rule language |
| Obligation | A statement the IR cannot express yet; names the missing construct and counts as unchecked ([D-049](design-decisions.md)) |
| Measured rule | A rule binding a metric on a subject to a threshold, verified by a benchmark, test or telemetry query |
| Never-allowed operation | A default-deny operation no rule allows; reported apart from conflicts as a vocabulary choice |
| Open conflict | A conflict found when an existing spec corpus was adopted, pinned and tolerated by the gate until resolved |
| Owner | The one person, or group acting as one, who approves a registry's rules, vocabulary, decisions and baseline ([D-056](design-decisions.md)) |
| Slice | The transitive set of rules, decisions and invariants a change can affect |
| Relation to many | A relation to several instances, checked over a bounded number of other instances with all, any and count ([D-050](design-decisions.md)) |
| Snapshot | A content-addressed version of the registry, produced by each merge |
| System spec | The registry: every spec statement codified as typed rules over one vocabulary, checked as one system |
| Synonym | A reviewed mapping from prose wording to a symbol |
| Theory tier | The solver theories a context opts into; core is always decidable, tiers 1 and 2 add expressiveness and are labelled in results |
| Unsat core | The minimal set of rules that are jointly inconsistent |
