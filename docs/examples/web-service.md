# Example: SaaS backend

A subscription product with billing, identity and privacy areas, each written as its own specs. It is the core docs' running example (W2 in [08](../08-workflows.md#w2-conflict-with-a-rule-written-months-earlier)) in a billing setting. One owner approves the registry ([D-056](../design-decisions.md)).

## Contexts

| Context | Holds |
| --- | --- |
| `identity` | `Actor` extension (roles User, Support, Admin), `Tenant` |
| `billing` | `Account`, `Invoice` |
| `privacy` | Erasure requests |

## Rules

```yaml
id: B12
kind: permission
effect: deny
operation: Account.delete
when: self.openInvoices > 0
prose: "Accounts with unpaid invoices can't be deleted."
---
id: B20
kind: invariant
when: self.status = deleted
assert: self.openInvoices = 0
```

## The contradiction

Six months later, privacy writes: "Customers can close their account at any time for GDPR erasure." Grounding maps "close their account" to `Account.delete`, and the translator emits:

```yaml
id: P31
kind: permission
effect: allow
operation: Account.delete
when: actor = self.owner and request.kind = erasure
```

The checker reports:

> An owner making an erasure request on an account with 1 open invoice is denied by B12 (billing) and allowed by P31 (privacy). Cross-context conflict: a decision record is required, because in-context precedence does not apply.

## Resolution

1. The owner drafts decision D7: P31 supersedes B12 when `request.kind = erasure`.
2. The checker re-runs and fails invariant B20: an erased account would still have open invoices.
3. Billing adds a conditional effect to `Account.delete`: under erasure, open invoices move to an anonymized ledger, so `openInvoices` becomes 0.
4. The checker passes. The owner approves D7, after checking with legal, which pins `P31@1` and `B12@3`.

## Assurance levels

| Rule | Level |
| --- | --- |
| B12, P31, B20, D7 | Proved |
| "Account deletion completes in under 2 s" | Verified (`measured`, k6 benchmark) |
| "The cancellation flow should feel respectful" | Reviewed (`informal`) |
