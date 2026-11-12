# Examples

Worked examples from deliberately unlike kinds of software. They exercise the same domain-free core: no example uses a feature the others couldn't. They are illustrations of the design, not domain packs, and nothing in them is built into the registry. Some features they use are planned and not built: `final` rules, the bit-vector tier, decision tables, rule effective windows (`effectiveFrom`, `effectiveTo`), code anchors, extractors that read schemas and manifests, generated tests, and checking `measured` rules against tests or telemetry. [What is built](../README.md#status) says which.

Each example follows the same shape: the project, its contexts, a few rules, a contradiction introduced months later from another part of the project, what the checker reports, how it is resolved, and which assurance level each rule gets.

| Example | Kind of software | The cross-time contradiction | Features shown |
| --- | --- | --- | --- |
| [web-service.md](web-service.md) | SaaS backend | Erasure rights vs "accounts with unpaid invoices can't be deleted" | Cross-context decision, conditional effect, invariant |
| [embedded-controller.md](embedded-controller.md) | Elevator controller firmware | Inspection-mode jogging vs "the car never moves with a door open" | `final` safety rule, mode state machine, bit-vector tier, `measured` timing |
| [data-pipeline.md](data-pipeline.md) | Data platform | Nightly export to a US warehouse vs EU data residency | Data contracts, derivation, residency rules, freshness SLO |
| [infrastructure-policy.md](infrastructure-policy.md) | Cloud platform | An automation bot vs change approval and a holiday freeze | Time windows, scoped decision, service-account actors |
| [cli-tool.md](cli-tool.md) | Open-source CLI | A plugin's cache writes vs "dry-run never writes" | Library-style contracts, decision table completeness, maintainers acting as one owner |

The core docs use a neutral running example (`Resource`, `Hold`, rule R12). Rule and decision IDs in these examples are local to each one.
