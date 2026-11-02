# Example: cloud platform change policy

A platform group manages infrastructure as code; one owner approves the registry. Rules govern who may change what, when, and with whose approval. Entities are environments and resources; operations are plans and applies.

## Contexts

| Context | Holds |
| --- | --- |
| `sre` | `Environment`, change approval policy |
| `release` | Freeze calendar |
| `automation` | Bots and their service accounts |

The project extends `platform.Actor` with `kind: [engineer, bot]` and `role: [developer, sre, releaseManager, certRotator]`. Extractors read Terraform plans, Kubernetes manifests and the CI system's service accounts.

```yaml
entity: Change
context: sre
attributes:
  environment: { type: ref Environment }
  approvals:   { type: int, range: [0, 10] }
  kind:        { type: enum, values: [config, scale, certRotation, schema] }
---
operation: Change.apply
actors: [developer, sre, releaseManager, certRotator]
default: deny
```

## Rules

```yaml
id: P1
kind: permission
effect: deny
operation: Change.apply
when: self.environment.tier = prod and self.approvals < 2
prose: "Production changes need two approvals."
---
id: R5
kind: permission
effect: deny
operation: Change.apply
when: self.environment.tier = prod
effectiveFrom: 2026-12-20
effectiveTo:   2027-01-04     # half-open: Jan 4 is not frozen
prose: "Production is frozen from 20 December to 3 January."
```

## The contradiction

A year later, automation writes: "The certificate-rotation bot may apply to production at any time without approval." The translator emits an allow for `Change.apply` when `actor.role = certRotator`.

The checker reports two conflicts:

> 1. A bot change to prod with 0 approvals is allowed by B2 (automation) and denied by P1 (sre).
> 2. A bot change to prod on 2026-12-24 is allowed by B2 and denied by R5 (release).

## Resolution

The owner decides the bot is safe only for certificate rotation, so B2 is narrowed and one scoped decision records the exception:

```yaml
id: B2
version: 2
kind: permission
effect: allow
operation: Change.apply
when: actor.role = certRotator and self.kind = certRotation
---
id: D3
resolves: [B2@2, P1@1, R5@1]
supersede:
  winner: B2
  losers: [P1, R5]
  when: self.kind = certRotation
rationale: "Expired certificates cause outages; rotation is low-risk and audited."
approvedBy: platform-lead@org
```

A later attempt to widen B2 to `self.kind in {certRotation, config}` makes D3 stale and re-opens both conflicts.

## Assurance levels

| Rule | Level |
| --- | --- |
| P1, R5, B2, D3 | Proved |
| "Monthly cloud spend stays under budget" | Verified (`measured`, billing export) |
| "Change descriptions should explain the why" | Reviewed (`informal`) |
