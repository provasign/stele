# Example: data platform

A data platform with ingestion, analytics and data-governance areas, each specified separately; one owner approves the registry. Entities are datasets and jobs; operations are exports and transforms.

## Contexts

| Context | Holds |
| --- | --- |
| `governance` | Classification and residency policies |
| `ingest` | `Dataset` (raw event tables) |
| `analytics` | Warehouse jobs, derived datasets |

Extractors: dbt manifests and Avro schemas for datasets and columns, Airflow DAGs for jobs, the warehouse catalog for regions.

```yaml
entity: Dataset
context: ingest
attributes:
  classification: { type: enum, values: [public, internal, personal, anonymized] }
  residency:      { type: enum, values: [EU, US, APAC, any] }
  tags:           { type: set, values: [events, billing, logs, ml] }
  # Request extended with targetRegion: enum[EU, US, APAC]
anchors:
  - { kind: table, ref: raw.events_eu, file: dbt/models/raw/events_eu.sql }
---
operation: Dataset.export
actors: [job, analyst]
default: deny
coverage: { required: [job] }
```

## Rules

```yaml
id: G3
kind: permission
effect: deny
operation: Dataset.export
when: self.classification = personal
  and self.residency = EU
  and request.targetRegion != EU
prose: "Personal data resident in the EU never leaves the EU."
```

## The contradiction

Eight months later, an analytics spec says: "A nightly job copies all event tables to the US warehouse for global reporting." Grounding maps "event tables" to `Dataset where tags contains events`, and the translator emits an allow for `Dataset.export` for actor kind `job` with `request.targetRegion = US`.

The checker reports:

> The nightly job exporting `raw.events_eu` (personal, EU) to the US is allowed by A7 (analytics) and denied by G3 (governance).

## Resolution

The owner decides that global reporting needs only anonymized data. Instead of a decision record overriding G3, the analytics spec gains a derived dataset and its rule narrows:

```yaml
id: A7
version: 2
kind: permission
effect: allow
operation: Dataset.export
when: self.tags contains events
  and self.classification = anonymized
  and request.targetRegion = US
```

The checker passes with no decision needed: the guards are now disjoint. A `derivation` rule declares that `events_global` is produced from `events_eu` by the anonymizing transform, so its classification is `anonymized`.

## Assurance levels

| Rule | Level |
| --- | --- |
| G3, A7, the derivation | Proved |
| "Raw events land within 2 h" | Verified (`measured`, warehouse freshness query) |
| "Row counts don't drop more than 10% day over day" | Verified (`measured`, data test) |
| "Dashboards should be understandable by sales" | Reviewed (`informal`) |
