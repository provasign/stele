# 05. IR and semantics

The IR has the primitive types programs have, so every statement a spec makes about values can be written down ([D-050](design-decisions.md)). Each type comes with operations the solver always decides; a rule that goes beyond them is still accepted, and a check the solver cannot decide is reported as `unknown`, never as a pass. Collections and relations to many are checked up to a declared bound, and results over them hold up to that bound.

## Expression grammar

```text
expr     := implies
implies  := or ('implies' implies)?               # right-associative
or       := and ('or' and)*
and      := not ('and' not)*
not      := 'not' not | cmp
cmp      := arith (cmpop arith | 'in' set | 'contains' arith)?
cmpop    := '=' | '!=' | '<' | '<=' | '>' | '>='
arith    := term (('+' | '-') term)*
term     := postfix ('*' INT)?                    # linear only
postfix  := atom ('[' expr ']')*                  # map lookup: self.limits[gold]
atom     := path | literal | ENUM | 'none' | call | quant | '(' expr ')'
literal  := INT | DECIMAL | STRING | DURATION | TIME | 'true' | 'false'
call     := 'startsWith' '(' expr ',' expr ')' | 'endsWith' '(' expr ',' expr ')'
          | 'contains' '(' expr ',' expr ')' | 'len' '(' expr ')' | 'matches' '(' expr ',' STRING ')'
          | 'isSet' '(' expr ')' | 'has' '(' expr ',' expr ')' | 'old' '(' expr ')'
          | ('ran' | 'runs' | 'lastRun') '(' OPERATION ')'
          | ('seconds' | 'minutes' | 'hours' | 'days') '(' expr ')'   # a whole number of units as a duration
          | ('min' | 'max') '(' expr ',' expr ')' | 'abs' '(' expr ')'
quant    := ('all' | 'any' | 'count') '(' IDENT 'in' (postfix | instances) (':' | '|' | ',') expr ')'
instances := 'instances' '(' ENTITY_NAME ')'               # every other instance of the entity
path     := root ('.' IDENT)*
root     := 'self' | 'actor' | 'request' | 'result' | ENTITY | IDENT   # ENTITY: lower-camel entity name; IDENT: a quantifier's variable
set      := '{' value (',' value)* '}'
DECIMAL  := INT '.' DIGITS                        # 1.5, -0.25
STRING   := '"' chars '"'                         # \" and \\ escape
DURATION := INT ('ms' | 's' | 'm' | 'h' | 'd' | 'w')   # milliseconds
TIME     := YYYY-MM-DD | RFC 3339 time            # milliseconds since the Unix epoch, UTC
```

Changes from the original grammar ([D-007](design-decisions.md), [D-050](design-decisions.md)):

- `implies` is the lowest-precedence operator ([D-041](design-decisions.md)).
- A root may name an entity in lower camel case, such as `config.sessionIdleTimeoutMinutes` or `chatSession.status`, for rules and decisions with no `self`. The root names that entity's single instance ([D-040](design-decisions.md)).
- Paths traverse relations and record fields: `self.owner.tenant.tier`, `self.result.code`.
- `all`, `any` and `count` replace `forall`, `exists` and `count(... where it ...)`; `forall` and `exists` are still read as `all` and `any`. The separator before the condition may be `:`, `|` or `,` (YAML needs quotes around `: ` in a plain value; registries Stele writes are quoted for it).
- A quantifier's variable written alone, as in `all(t in self.tags: t = urgent)`, is the element, ahead of any enum value of the same name.
- `now` is the clock; `old(x)`, `ran`, `runs` and `lastRun` describe the past ([D-051](design-decisions.md), [Order and history](#order-and-history)).
- `result` is what a step produces, `instances(E)` every other instance of an entity, and `seconds`, `minutes`, `hours`, `days`, `min`, `max` and `abs` are exact arithmetic ([D-064](design-decisions.md), [What a step produces](#what-a-step-produces), [Aggregates](#aggregates)).
- A permission or precondition may name `operation: "*"` (every operation) or `"Workspace.*"` (every operation of an entity); with `"*"` the rule uses roots such as `request`, not `self`.

### Types

| Type | Declared as | Lowering | Operations the solver always decides |
| --- | --- | --- | --- |
| `bool` | `{ type: bool }` | Bool | everything |
| `int` | `{ type: int, range: [0, 10] }`, range optional | Int, with the range asserted | `+`, `-`, `* K`, comparisons |
| `decimal` | `{ type: decimal, range: [0, 1] }`, range optional | Real (exact) | `+`, `-`, `* K`, comparisons, mixed with ints |
| `enum` | `{ type: enum, values: [a, b] }` | datatype | `=`, `!=`, `in` |
| `string` (also `bytes`, `text`) | `{ type: string }` | String | `=`, `!=`, `in`, `startsWith`, `endsWith`, `contains`, `len`, `+` with literals and attributes |
| `duration` | `{ type: duration }` | Int, milliseconds | with durations: `+`, `-`, comparisons |
| `time` (also `date`, `timestamp`) | `{ type: time }` | Int, milliseconds since the epoch, UTC | time ± duration, time − time, comparisons |
| `list` | `{ type: list, of: string, maxLength: 5 }` or `list<string>` | a length and one variable per position, up to `maxLength` (default 3) | `contains`, `len`, `isEmpty`, `xs[i]` (unset past the end), `=` with a list or a literal `["a", "b"]`, `all`/`any`/`count` |
| `set` | `{ type: set, of: enum, values: [...] }` or `set<enum[a, b]>`; `set<string>` | enum or bool elements: array to Bool, over every value; other elements: a length and one variable per position up to `maxLength` (default 3), where repeats count once | `contains`, `len`, `isEmpty`, `all`/`any`/`count`, `=` (regardless of order) |
| `map` | `{ type: map, key: enum, keyValues: [...], of: int }` or `map<enum[gold, silver], int>` | array, plus the set of present keys | `m[k]`, `has(m, k)` |
| optional | `optional: true` on any type, or `optional<T>` | the value plus a Bool for its presence | `isSet(x)`, `x = none` |
| record | `type: record` with `fields:`, or dotted names (`result.code`) | its fields, as an entity owned by the attribute's | fields as attributes: `self.result.code` |
| relation to many | `relations: { workspaces: { to: Workspace, many: true, max: 3 } }` | `max` other instances (default 3), each with its own attributes and a membership flag | `all`/`any`/`count` over it |
| optional relation | `relations: { project: { to: Project, optional: true } }` | a Bool for whether it leads anywhere; operations that list it in `writes` change it | `isSet(self.project)`, `self.project = none` |
| regular expressions | `matches(self.slug, "[a-z0-9-]+")` | Z3 regular languages; the whole string must match | literals, `.`, classes, `*`, `+`, `?`, `{m,n}`, `\|`, groups; may be `unknown` combined with lengths |

Element types of lists, sets and maps are scalar types; collections of collections are modeled as relations to many entities. Generic spellings (`list<string>`, `map<string, int>`, `optional<int>`, inline `enum[a, b]`) are accepted, so translators can write a type in one field. Operations write record fields through the record: `writes: [Job.result.code]`.

Semantics of the less familiar cases:

- **Optional values.** An unset optional value, or a value reached through an unset optional relation, equals and compares with nothing: `self.owner = "bob"` is false while `self.owner` is unset, and `self.owner != "bob"` true ([D-052](design-decisions.md)). `isSet` and `= none` test presence. Comparing a value with its own earlier value, `x = old(x)`, says the step keeps it: an optional value unset before and after is kept.
- **Other instances.** A state invariant about one instance that reads only its own attributes and quantifies over nothing ("every tool's id is positive") also holds, before a step, for the other instances a rule ranges over with `instances(E)`.
- **Keys.** A uniqueness invariant, `all(u in instances(E): u.k != self.k)`, makes `k` a key: before a step, two values of `E` with the same `k` (the step's own instance, the `instances(E)` slots, the items of a relation to many) are one instance and agree on every attribute, when both are present. A result per item keyed by `k` then has one value per key.
- **Undecided steps.** A step the solver cannot decide fails the gate unless the registry records, with an `unchecked: <step>` document written when the owner approved its rules knowing that, that it stays unchecked.
- **Lists** are checked up to `maxLength`. A rule over a longer list is checked for lists up to that length only.
- **Relations to many** denote `max` instances other than the single instance roots and `self` name, each with its own attributes. An operation acts on its own instance, so it never changes theirs; from an instance in the collection, the inverse relation (declared with `inverse:`) leads back to the instance that holds it. "Every other workspace on the node keeps running" is `all(w in self.node.workspaces: w.status = running)`.
- **Strings** are decided by Z3's string solver. Equality, prefix, suffix and containment against literals and concatenation are decided; arbitrary combinations with lengths may come back `unknown`, which the checker reports as such.

Enum and set values are written `{ type: enum, values: [a, b] }`. The earlier `{ type: enum[a, b] }` is not valid YAML: inside a flow mapping the comma ends the value ([D-038](design-decisions.md)).

Enum attributes may be compared with `=` and `!=` when they declare exactly the same nonempty set of value names, regardless of declaration order. Equality means matching names: `a.status = b.status` expands into a disjunction of `(a.status = value and b.status = value)` for each declared value. The attributes keep separate SMT datatypes; the compiler never compares their ordinal positions or guesses synonyms. Different value sets remain a type error and require an explicit mapping ([D-048](design-decisions.md)).

### Not yet expressible

Non-linear arithmetic (a variable times a variable, exponentials), recursion, user-defined functions, interleaving concurrency, graph traversal, geometry and calendar arithmetic ("first working day of the month") are not in the IR yet. Statements that need them are recorded as an `obligation` that counts as unchecked until the IR can express them ([D-049](design-decisions.md)).

### Planned extensions

The trial translations show what specs state that the IR still cannot ([D-049](design-decisions.md)). Each extension gets its own decision before it is implemented, and each must keep a decision procedure:

| Extension | Statements it covers | Likely check |
| --- | --- | --- |
| Future-time ordering over bounded traces | "eventually READY or FAILED" with no deadline, "no unbounded restart loop" | Bounded model checking over the transition system of D-042, results labelled with the trace bound; past-time order and deadlines are already expressible ([Order and history](#order-and-history)) |
| Snapshot references | "Startup is unchanged from before this fix" | The current rules over a named behavior must be equivalent to those in an earlier snapshot; from Phase 2 also emits a snapshot test |
| Repeated and combined operations | "`--cleanup` is idempotent", atomic staging | Running an operation twice equals running it once; a failed composite leaves the state unchanged |

Multiple instances with quantifiers, strings, records and the other value types of this list are implemented ([D-050](design-decisions.md)).

## Rule kinds

| Kind | Meaning | Checked by |
| --- | --- | --- |
| `permission` | `allow` (it must run) or `deny` (it must not) an operation when the guard holds, or `possible`: it must be able to run in some situation where the guard holds, with every other rule ([D-052](design-decisions.md)) | Conflict, gap, vacuity; reachability for `possible` |
| `precondition` | The operation must not run unless the guard holds, for every actor; violations are errors, not denials | Invariant preservation, reachability |
| `invariant` | `assert` holds in every reachable state where `when` holds | Invariant preservation |
| `derivation` | Defines a derived attribute or a decision-table output | Type check, range check |
| `measured` | A metric on a subject stays within a threshold | Threshold-conflict check; verified by tests or telemetry ([domain-coverage](domain-coverage.md#assurance-levels)) |
| `obligation` | A requirement about the code or the development process (tests exist, CI runs, a signature), or behavior the IR cannot express yet | None; counted as unchecked; checked against the repository from Phase 2 |
| `informal` | A judgment no check can decide; reviewed by its owner for each text version | Recorded review (spec gate) |

A precondition differs from a deny rule in two ways ([D-024](design-decisions.md)): it cannot be superseded by a decision, and a generated runtime guard reports it as a failed precondition, not as an authorization denial.

## Situation and response

The registry describes a transition system ([D-042](design-decisions.md)). The **state** is every attribute of every entity. An **operation** is one step: an actor invokes it (a user, a scheduler, an agent, another service) and the system handles it. Events such as "an agent finished" or "the idle sweep ran" are operations too.

Each operation declares:
- `writes`: the attributes the step may change. Every other attribute keeps its value (the **frame**).
  An operation an entity names in `createdBy` writes every attribute of that entity the world does not set: a new instance has no earlier values to keep.
- `default`: whether the operation runs when no permission rule matches.
- optionally an `effect` (`{status: stopped}`). Transitions in a `stateMachine` add a start-state precondition and an effect.

An attribute that some operation writes is **system-controlled**. Every other attribute (clocks, counters, configuration) is set by the environment. Nothing is declared for this: it follows from `writes`, after Jackson and Zave's split between environment- and machine-controlled phenomena.

Rules read the state the way specs are written:

| Rule | Meaning |
| --- | --- |
| `invariant` when W assert A, no `on` | A holds in every state where W holds |
| `invariant` `on: op` when W assert A | when op runs from a state where W holds, the state after it satisfies A. W reads the state before and the request; A reads the state after |
| `permission` allow op when G | when G holds before the step, op runs |
| `permission` deny op when G | when G holds before the step, op does not run |
| `precondition` op when P | op runs only if P holds before the step |
| operation `default` | with no matching allow (default deny), op does not run; with no matching deny and every precondition holding (default allow), op runs |
| decision: supersede L when C | L applies only where C does not hold |
| decision: retire L | L is dropped |
| derivation `X.default = v` | configured fallback value; ordinary checks cover all values. `stele check --assume-defaults` assumes X = v only for attributes no operation writes; written defaults remain initial values, which these checks do not model |

The checker asks two kinds of question, each as "is there a situation for which no response satisfies every rule?":

  ∃ situation. ∀ response. ¬(rule₁ ∧ … ∧ ruleₙ)

1. **State.** The situation is the environment attributes; the response is the system-controlled attributes; the rules are the invariants. A conflict means some situation has no consistent state.
2. **Each operation.** The situation is any state satisfying the invariants, plus a request. The response is whether the operation runs and the values of what it writes. The rules are the operation's permissions, preconditions, effect and step invariants, plus every invariant that reads something it writes, which must still hold afterwards. A conflict means the operation cannot be handled correctly from that state.

A permission conflict is one case: an allow and a deny that both match leave no value for "runs". So are obligations that cannot both be met, such as "destroy the workspace" and "keep it alive for inspection". The checker reports each conflict as a minimal set of rules (the unsat core) with a counterexample, and enumerates every minimal set ([06](06-checker.md#conflict-enumeration)). It also reports **dead responses**: a state value no state can have, or an operation that can never run.

Writes make this scale. An invariant that reads nothing an operation writes holds after that operation by the frame, so each operation is checked only against the invariants that touch its writes, together with the invariants connected to them through shared attributes, before the step. Checks split per operation and grow with what a change touches, not with the registry.

The step check is one step deep: it assumes the state before can be any state the invariants allow, which over-approximates the reachable states. A conflict can therefore be reported from a state that no sequence of operations reaches. Where every instance in that state is created by an operation its entity names in `createdBy`, the checker asks whether any trace can enter the state: a creating step that leaves it behind, or a step that changes into it from a state that differs. The state includes whether each optional value is set. If no step can, under every rule, the conflict is reported as unreachable and does not fail the check (ISSUES 3). An entity without `createdBy` may start in any state, so its values are left out of that state: if no trace reaches the states the rest describes, none reaches the conflict's. During `stele change`, a conflict in a step of an entity with no `createdBy` becomes a `creator` question for the agent when naming one of the entity's steps as its creator would set the conflict aside; the answer is recorded as `createdBy`. Bounded multi-step checking from declared initial states (reachability, [06](06-checker.md)) would remove the rest.

A rule that reads nothing any operation writes cannot be made true by the system. It is read as an environment assumption, with a warning.

A contradiction is reported once, where it is, and does not hide the rest:

- Assumptions (environment assumptions and declared defaults) are checked together in their own unit. Those that conflict with each other are reported there, and every other unit is checked without them, as a compiler goes on after an error; otherwise no situation would exist and every other conflict would pass unseen.
- State invariants that contradict each other are the state unit's conflict. An operation's unit assumes only the invariants connected to its own rules through shared attributes, so a contradiction about attributes it does not touch leaves its question unchanged, and its own conflicts are still reported. A contradiction about what it touches leaves it no state to start from.

`self` denotes the rule's subject: the entity of its operation, its `subject:` field, or the one entity that has every attribute or relation the rule reads from `self`. If several entities qualify, the compiler asks for `subject:` and does not guess ([D-040](design-decisions.md)). `request` exists only in rules about an operation.

v0 implements this section and the core tier, without aggregates, dates, durations, `contains` or multi-step reachability. Steps 3 and 4 below (final rules, more-specific-wins) are not implemented yet: v0 reports every allow/deny overlap as a conflict.

## Rule semantics

For an operation `op`, actor `a`, request `r` and state `s`, the effective decision is computed in this order ([D-001](design-decisions.md)):

1. **Preconditions.** If any precondition of `op` fails, the result is `error`.
2. **Explicit decisions.** For each active decision whose `supersede.when` holds, remove the losing rule for this state.
3. **Final rules.** If any matching `final` deny remains, the result is `deny`. If any matching `final` allow remains and no `final` deny matches, the result is `allow`.
4. **Implicit precedence, within one context.** If the context enables `moreSpecificWins`, a matching rule whose guard strictly implies another matching rule's guard, both from that context, removes the other for this state. The solver proves the implication, so this step is deterministic.
5. **Deny wins.** If any matching deny remains, the result is `deny`.
6. **Allow.** Else if any matching allow remains, the result is `allow`.
7. **Default.** Else the operation's `default` applies.

Steps 3 to 6 apply to rules with status `active` or `deprecated`.

### Conflicts

A **conflict** is a situation for which no response satisfies every active rule, after decisions and in-context precedence ([D-039](design-decisions.md), [situation and response](#situation-and-response)). The order above says what a running system returns; the conflict check says whether the rules can all be met. Three cases matter here:

- A matching allow and a matching deny that both remain after steps 2 to 4.
- An allow whose guard admits a state where a precondition fails. The allow says the operation runs there, the precondition says it must not. At runtime step 1 returns `error`, but the registry reports the conflict, because "users MUST be able to send follow-ups" beside a precondition that blocks them after completion is a real contradiction (FR-026 + FR-028 in the Project A registry). Many such findings share one precondition, such as authentication, and are grouped by it ([W5](08-workflows.md#w5-adopting-an-existing-spec-corpus)).
- Obligations that cannot all be met, such as two effects or step invariants that disagree.

Further rules:

- Deny-wins (step 5) is the runtime tiebreak for states that slipped past the gate, such as a deprecated rule or code that leads its spec. The registry still reports every conflict, because silent deny-wins is the hidden contradiction this system exists to surface.
- Implicit precedence never crosses contexts. A conflict between rules of two contexts always needs a decision record.
- Two `final` rules with opposite effects that can both match are an error no decision can resolve.
- Every conflict resolved by step 4 is reported with the label `resolved: moreSpecificWins`, so nothing is silent.

### Example

R12 (resources): `deny Resource.delete when self.activeHolds > 0`.
R31 (privacy): `allow Resource.delete when actor = self.owner and request.kind = erasure`.

State: actor is the owner, request is `erasure`, `activeHolds = 1`. Both match after step 4, because they belong to different contexts. That is a conflict. D7 removes R12 when `request.kind = erasure`, so step 6 returns `allow`.

## Time

Times and durations are integers in milliseconds, which keeps them in linear arithmetic ([D-025](design-decisions.md), [D-050](design-decisions.md)).

- `seconds(n)`, `minutes(n)`, `hours(n)` and `days(n)` turn a whole number into a duration, so a setting given in seconds compares with durations: `now - lastRun(Job.sweep) <= seconds(config.intervalSeconds)`. `min(a, b)`, `max(a, b)` and `abs(a)` take numbers, durations or times (abs: numbers and durations) and are exact ([D-064](design-decisions.md)).

- `time` attributes hold instants (UTC); dates such as `2025-01-01` and RFC 3339 times are literals. `duration` attributes and literals such as `30s`, `15m`, `2h`, `7d` are spans: `request.at - self.createdAt <= 30d`.
- A time plus or minus a duration is a time; the difference of two times is a duration. Durations do not compare with plain numbers: write `15m`, not `15`.
- Grandfathering is a guard on a creation date: `allow legacyLimits when self.createdAt < 2025-01-01`.
- Rule effective windows are half-open: `[effectiveFrom, effectiveTo)`. Two rules whose windows touch at a boundary do not overlap. The checker verifies consistency per window.
- Rule lifecycle (`active`, `deprecated`) says whether a rule is part of the registry. Effective windows say when an active rule applies in domain time. They are independent.
- Recurring schedules and calendar arithmetic ("first working day of the month") are an `obligation` in v1.

## Precedence the text states

A rule may name the statements it `overrides` when its own text states the precedence: "this applies regardless of the `--force` flag", "per FR-005, not when `--dry-run` is set" ([D-052](design-decisions.md)). Where the overriding rule's condition holds, the overridden statement's rules do not apply, in the units where both apply; the precedence is shown on the rule. Precedence the text does not state is never inferred: those conflicts are reported for the owners, as D-001 requires.

## Order and history

Statements about order and time are checked one step at a time, like every other rule, through history attributes and a clock ([D-051](design-decisions.md)). This is the usual encoding of past-time temporal properties: what happened before is part of the state.

- `now` is the clock, a time the world sets.
- `ran(Op)` is whether the operation has happened, `runs(Op)` how often (up to 1,000,000), and `lastRun(Op)` the clock when it last ran (optional: unset until it has). Running the operation sets all three; no other operation changes them. They exist for an operation once a rule reads them.
- `old(x)` in the `assert` of a rule `on:` an operation is `x` before the step; elsewhere it is `x`.
- `assume: true` on an invariant `on:` an operation makes it a fact about the situation the step runs in, not a requirement: `when` and `assert` both read the state before the step and the request, and the check assumes it, as it assumes state invariants before a step. A glossary's definition of a term by request fields is one (ISSUES 62).

| Statement | Rule |
| --- | --- |
| Clone before starting | precondition `Workspace.start` when `ran(Workspace.clone)` |
| Run every five minutes | invariant `isSet(lastRun(Job.sweep)) implies now - lastRun(Job.sweep) <= 5m` |
| Refresh before the token expires | invariant `self.active implies now < self.tokenExpiresAt` |
| Each delay doubles | invariant `on: Retry.again`, `self.delay = old(self.delay) * 2` |
| Stopping one workspace does not disrupt the others | an invariant over what they share, `all(w in node.workspaces: w.status = running implies node.vmExists)`; an operation never changes the other instances |

Like invariants, history is checked from every state the invariants allow, not only states reachable from a start: a rule may need an invariant relating history to state (a running workspace was cloned) to rule out states that cannot happen.

## What a step produces

`result` is what a step produces and the system keeps nowhere else: what it returns, the messages, notifications, events, logs and metrics it emits, how many attempts it made ([D-064](design-decisions.md)). Its fields are attributes of the built-in entity `Result`, added like request fields: `extend: platform.Result`.

- A step decides its result, fresh each time: in the step, `result.x` is part of the response, like what the step writes, but no frame keeps it from before and no operation lists it in `writes`.
- `result` is read only in the `assert` of a rule `on` an operation. In a state invariant, a guard, a `when` or `old(...)` it is a type error: there is no result before the step.
- Two rules that demand different outputs of one step conflict like any two rules on its response: "cancelling notifies the customer" against "cancelling never sends a notification".

| Statement | Rule |
| --- | --- |
| Cleanup returns the number of deleted entries | invariant `on: Cache.cleanup`, `result.deletedCount >= 0` |
| No email is sent when an order is cancelled | invariant `on: Order.cancel`, `not result.emailSent` |
| Retry at most three times | invariant `on: Job.run`, `result.attempts <= 3` |
| Both succeed or both roll back | invariant on the step, `(result.ok implies self.a and self.b) and (not result.ok implies self.a = old(self.a) and self.b = old(self.b))` |

## Aggregates

`all`, `any` and `count` range over a relation to many, a list, or a set of enum or bool values ([D-050](design-decisions.md)).

- A relation to many is checked over its `max` instances (default 3) other than the one roots and `self` name. Each has its own attributes and a flag saying whether it is in the collection, so `count(w in node.workspaces: w.status = running)` counts members.
- A list is checked up to its `maxLength` positions; a set of enum or bool values over every value it may hold.
- A relation's bound is raised to fit the largest count a rule compares it with (`count(...) = 18` is checked over at least 19 instances), up to 64; the raise is noted in the check's warnings ([D-052](design-decisions.md)).
- Where the real bound is unknown, the author chooses one, and every result over that relation is valid up to that bound.
- `instances(E)` is every other instance of entity `E`, related or not, checked like a relation to many: over `DefaultMaxInstances` (3) instances other than the one roots and `self` name, raised to fit a count a rule compares ([D-064](design-decisions.md)). "Account numbers are unique" is `all(a in instances(Account): a.number != self.number)`. Whether other instances exist is the world's: a requirement that some must exist cannot be met by the system alone.
