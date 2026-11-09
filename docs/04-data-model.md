# 04. Data model

Contexts group entities, entities expose operations, rules constrain operations, and decisions resolve conflicts between rules. Synonyms, open conflicts and decision tables support those five core kinds. The registry has one owner, who approves everything in it ([D-056](design-decisions.md)).

| Kind | Purpose | Lives in |
| --- | --- | --- |
| Context | A namespace grouping related vocabulary; scopes in-context precedence | The registry |
| Entity | A typed thing with attributes and an optional state machine | One context |
| Operation | A state change with actors, a default effect, writes and coverage | The entity's context |
| Rule | A typed constraint on an operation or the state | One context |
| Decision | A versioned precedence ruling between rules | The registry |
| Synonym | Maps prose wording to a symbol | The registry |
| Open conflict | An acknowledged conflict from adopting an existing spec corpus, tolerated until resolved | The registry |
| Decision table | A tabular authoring form lowered into rules | One context |

## Repository layout

What `stele init` and the checks write, committed with the code:

```text
stele.yaml                        # the registry, the spec format, the agents set up
.stele/
  registry/
    context.yaml                  # the project's context
    adopted.yaml                  # rules read from the specs adopted at first
    adopted.vocabulary.yaml       # their entities, attributes and operations
    adopted.world.yaml            # facts about the world the rules assume
    change-<id>.yaml              # one file per approved change: its rules,
    change-<id>.vocabulary.yaml   #   and the vocabulary it added
    lessons/L-3f2a9c.yaml         # what the owner taught Stele (D-063), one file each
    approvals/2026-11-09T003922Z-0324d7.jsonl   # one record per approval run
  accepted-statements.yaml        # the statements the registry was built from
  kept-intents.yaml               # Shale requests kept as requirements
  retired-intents.yaml            # requests retired, or set aside as not requirements
  cards/<change>.json             # the pull-request card for each change
  .gitignore                      # keeps change/, adopt/, audit/ and cache/ out of commits
```

The registry files are YAML documents of the kinds below, and a project may also split them by hand (`contexts/`, `domain/`, `rules/`, `decisions/`, `tables/`, `conflicts/`, `synonyms.yaml`): Stele reads every YAML file under the registry. One file per change, lesson and approval run keeps parallel branches from colliding.

## Context

A namespace for related vocabulary. Contexts keep a large registry readable and scope in-context precedence ([D-001](design-decisions.md)). They carry no ownership: any rule may read any attribute or constrain any operation ([D-056](design-decisions.md) supersedes the exports of D-002).

```yaml
context: resources
formatVersion: 1
owner: jane@acme          # optional label; the registry's owner approves everything
timezone: UTC
timeGranularity: day
owns: [Resource, Hold]
imports: [platform.Actor, platform.Request, identity.Tenant]
precedence:
  moreSpecificWins: true                # within this context only
```

## Entity

```yaml
entity: Resource
context: resources
attributes:
  status:   { type: enum, values: [active, locked, deleted], initial: active }
  owner:    { type: ref platform.Actor }
  region:   { type: enum, values: [US, EU, APAC] }
  createdAt: { type: date }
  classification: { type: enum, values: [public, internal, restricted] }
  retentionDays:  { type: int, range: [0, 3650], meaning: days a resource is kept after deletion }
  activeHolds:
    type: int
    range: [0, 1000]
    derived: count(self.holds where it.state = active)
relations:
  holds: { to: Hold, inverse: resource, bound: 1000 }
stateMachine:
  field: status
  terminal: [deleted]
  transitions:
    - { from: active, to: locked, via: lock }
    - { from: locked, to: active, via: unlock }
    - { from: [active, locked], to: deleted, via: delete }
anchors:
  - { kind: table, ref: resources, file: db/migrations/0004_resources.sql }
```

- `meaning` is optional: the fact the attribute stands for, in plain words. The translator shows it beside the attribute so prose in different words maps onto the same symbol; it has no effect on checking.
- An attribute some operation `writes` is system-controlled; every other attribute (clocks, counters, configuration) is set by the environment ([05](05-ir-and-semantics.md#situation-and-response)).
- `setBy: world` marks a value the world drives even though a step may reset it (minutes until shutdown: lowered by time, reset by activity). A step lists it in `writes` only with a fixed value in `effect` (`effect: { minutesLeft: 60 }`), so the step cannot choose the value to avoid a rule; before every step the world sets it (ISSUES 19).
- Each `stateMachine` transition names an operation (`via: delete` is `Resource.delete`), which then runs only from the `from` states and leaves the field at `to`.
- `derived` attributes are modeled by the solver as abstract bounded integers with declared constraints. The checker reasons about individual related elements only when a rule quantifies over them ([D-026](design-decisions.md)).
- Every relation declares a `bound`. An aggregate over an unbounded relation is a compile error.
- `terminal` states are exempt from the deadlock check ([D-016](design-decisions.md)).

### Built-in actor and request entities

There is exactly one actor model ([D-004](design-decisions.md)). The built-in `platform` context provides only skeletons with no domain content; each project declares what its actors and requests are.

```yaml
# Built in. The registry ships nothing else.
entity: Actor
context: platform
attributes:
  kind: { type: enum, values: [] }     # declared by the project: human, service, device, scheduler...
  role: { type: enum, values: [] }     # declared by the project
---
entity: Request
context: platform
attributes:
  at: { type: date }
```

A project extends them in a context it owns. In the running example, an `identity` context does this:

```yaml
extend: platform.Actor
context: identity
attributes:
  kind:   { values: [human, service] }
  role:   { values: [User, Support, Admin] }
  tenant: { type: ref Tenant }
---
extend: platform.Request
context: identity
attributes:
  kind: { type: enum, values: [normal, erasure, cleanup] }
---
entity: Tenant
context: identity
attributes:
  tier:   { type: enum, values: [basic, standard, premium] }
  region: { type: enum, values: [US, EU, APAC] }
```

An embedded project would instead declare `kind: [operator, controller, sensor]`; a data pipeline, `kind: [job, analyst, serviceAccount]`. Roles are enum values, never separate entities, so the solver checks all roles at once. Parameterization by tier, tenant or region works the same way: attributes, not rule copies.

## Operation

```yaml
operation: Resource.delete
actors: [User, Admin]                 # values of platform.Actor.role, declared in identity
default: deny
writes: [Resource.status]
effect: { status: deleted }
conditionalEffects:
  - when: request.kind = erasure
    effect: { activeHolds: 0 }       # holds released to the anonymized archive
    via: Hold.archive
coverage:
  required: [User, Admin]             # each role must have at least one explicit rule
anchors:
  - { kind: http, ref: "DELETE /resources/{id}", file: api/resources.ts, line: 88 }
  - { kind: ui,   ref: "settings.deleteResource.button" }
```

- `default` makes the world closed: with no matching rule, the default applies. Events the system handles (a scheduled sweep, a message from another service) are operations too, usually with `default: allow`.
- `writes` lists every attribute the operation may change; the rest keep their value. It is required, and it is what lets checks split per operation.
- `extend: Resource.delete` with `writes: [...]` adds writes to an operation declared elsewhere: `stele change` uses it when a change's rule asserts something about the state after an existing step ([D-060](design-decisions.md)). With `keeps: [...]` it takes writes back: the operation keeps those values, whichever file listed them. `stele change` writes it when the agent says that an approved step only reports a value it was recorded as writing (a listing that shows each tool's name); a value the operation's `effect` sets cannot be kept.
- An entity (or an `extend` of it) may name the operations that create it: `createdBy: [Cluster.init]`. Such an operation never runs on an existing instance, so it writes every attribute of the entity that the world does not set, and nothing of an earlier state carries over ([D-059](design-decisions.md)). A clash in the creation itself still shows; one that only an existing instance could cause does not.
- `coverage.required` declares which roles must have an explicit rule. Gap detection checks only declared coverage ([D-015](design-decisions.md)).
- `conditionalEffects` let an operation's effect depend on the request, which is how D7 stays consistent with the deleted-resource invariant ([D-003](design-decisions.md)).

## Rule

```yaml
id: R12
version: 3
kind: permission          # permission | precondition | invariant | derivation | measured | obligation | informal
effect: deny
operation: Resource.delete
when: self.activeHolds > 0
final: false              # true = no rule or decision may override it (not built yet)
effectiveFrom: 2026-03-02 # optional; half-open [from, to)
explanation: user         # internal | user | redacted
status: active            # draft | proposed | active | deprecated | retired
source:
  statementId: resources/FR-012 # current imported identity; rule ID stays stable when it moves
  statementHash: sha256:9f2c... # latest imported text; separate from the translation's textHash
  doc: specs/prose/resources.md#resource-deletion
  prose: "Resources with active holds can't be deleted."
  approvedBy: jane@acme       # the owner's approval, pinned to textHash
  approvedExamples: [ex-41, ex-42, ex-43]
  textHash: sha256:9f2c...    # set by importers
  introduced: 2026-03-02      # date the text first appeared in git
tests: [T-R12-*]
```

Importers supply the text and introduction date; translation records them as `source.textHash` and `source.introduced`, alongside `source.statementId`. Sync compares imported statements by anchor and text hash. `sync --apply` updates `statementId`, `statementHash` and `doc` while preserving the rule ID, version and original translated text/hash. The latest imported hash distinguishes an already-applied sync from a positional ID reused by another statement, so repeating `--apply` does not shift provenance again. Later sync, review confirmation, triage and recall use the explicit statement identity; legacy rules fall back to their ID prefix. Adoption triage uses `introduced` to propose which statement supersedes which ([W5](08-workflows.md#w5-adopting-an-existing-spec-corpus)).

`subject` names the entity `self` denotes. It is needed only when the rule has no operation and more than one entity has the attributes it reads from `self`.

An invariant rule has no operation:

```yaml
id: R20
version: 1
kind: invariant
subject: Resource
when: self.status = deleted
assert: self.activeHolds = 0
status: active
```

Kind semantics are defined in [05](05-ir-and-semantics.md#rule-kinds).

## Decision

Decisions pin the exact rule versions they resolve. If either rule is edited, the decision becomes **stale** and the compiler re-opens the conflict ([D-017](design-decisions.md)).

```yaml
id: D7
version: 1
resolves: [R31@1, R12@3]
supersede:
  winner: R31
  loser: R12                         # or losers: [..] for one exception over several rules
  when: request.kind = erasure     # IR, not a string
rationale: "Erasure is a legal obligation; holds are released to an anonymized archive first."
approvedBy: jane@acme
```

## Synonyms

```yaml
platform.Actor: [user, member, owner]
Resource.delete: [remove, erase, purge]
```

Every new synonym is a reviewed change, including synonyms written by an auto-accepted mapping ([D-006](design-decisions.md)). A synonym that collides with another symbol's synonym is rejected.

## Open conflict

An open conflict records a conflict found when an existing spec corpus was adopted and left unresolved ([D-044](design-decisions.md), [W5](08-workflows.md#w5-adopting-an-existing-spec-corpus)). The gate tolerates exactly this set of rule versions and blocks every other conflict.

```yaml
id: OC-4
rules: [R-014-FR-009c@1, R-003-FR-023@1]   # one minimal conflicting set, pinned
response: null            # for a dead response: the response no situation allows
situation: "an idle running workspace whose user turned auto-stop off"
acknowledgedBy: [jane@acme]   # the owner
since: 2026-10-24
status: open              # open | closed
closedBy: null            # decision or spec PR that closed it
```

Open conflicts are created only in an adoption baseline (`stele baseline` writes them to `conflicts/adoption.yaml`). Editing a pinned rule makes the record stale, which is derived from the pinned versions rather than written: the conflict blocks again if it remains, and the gate reports the record resolved if it is gone. An operation's own constraints in a conflict (`Workspace.stop.effect`, `Workspace.stop.from`) are pinned by name, without a version.

## Lesson

A lesson is something the owner taught Stele about this project ([D-063](design-decisions.md)), saved as one file under the registry's `lessons/` and committed with the specs. Most are written on their own from the owner's answers in `stele change`; `stele lesson add` records one no question has asked.

```yaml
lesson: L-3f2a9c          # a hash of its content, so parallel branches never collide
kind: term                # term | fact | reading | scope
says: "Customer in the billing specs and Account in the support specs are the same person; use Account."
statements:               # optional pin: statement ID -> text hash; none = the whole project
  support/FR-9: 3f2a...
terms: [Customer, Account] # words for finding the requirements it concerns
from: "stele change c1: same-x"
by: jane
at: 2026-10-31
```

- `term`: two names for one thing, or one thing under two names in different specs. `fact`: true of the world in this project ("invoices are kept for seven years", "this situation cannot happen"). `reading`: how one requirement is to be read; always pinned. `scope`: how requirements here are read ("a record checked when it is created is enough unless the text names other moments").
- Every translation (`stele change`, `stele formalize`) gives each statement its pinned lessons and the project-wide ones that concern it as notes; the coverage judge follows `reading` and `scope` lessons before its own guidance. The report says which statement was read with which lesson.
- A pinned lesson stops applying when its statement's text changes, and `stele lesson list` marks it stale. Lessons on the same terms that say different things are warned about: git merges them cleanly, so Stele has to notice.
- Only the owner's own words become lessons: a note the agent passes with `--quoted`, as the owner said it. Answers the agent gives itself (Stele correcting its own misreading) and notes it wrote do not. A "cannot happen" lesson keeps the owner's words, not the solver's situation.

## Decision table

Not built yet: the loader does not read tables. Decision tables are an authoring form for dense rule sets. Each row lowers to one rule; the checker treats rows as ordinary rules, plus two table checks: overlapping rows and missing combinations ([D-022](design-decisions.md)).

```yaml
table: retention-period
context: resources
operation: Resource.archive
inputs:  [self.owner.tenant.tier, self.classification]
outputs: [self.retentionDays]
hitPolicy: unique         # unique | first; unique = overlaps are errors
rows:
  - { tier: basic,              classification: [public, internal], retentionDays: 30 }
  - { tier: basic,              classification: restricted,         retentionDays: 90 }
  - { tier: [standard, premium], classification: "*",               retentionDays: 365 }
```

Quantities with units (money, durations, sizes, rates) are scaled integers with a declared unit, such as cents, milliseconds or bytes, which keeps them in linear integer arithmetic. Decimal and floating-point types are not in the core IR; projects that need them opt into the real-arithmetic theory ([domain-coverage.md](domain-coverage.md#theory-tiers)).
