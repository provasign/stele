# Example: open-source CLI tool

An open-source command-line tool with core maintainers and community plugin authors. The core maintainers, acting as one, are the owner and decide intended behavior. Operations are subcommands; attributes are options and the file system state they touch.

## Contexts

| Context | Holds |
| --- | --- |
| `core` | `Invocation` (options), `Workspace` (files, cache) |
| `sync-plugin` | The `sync` subcommand |

Extractors: the argument parser definition (click, clap or cobra) yields options and their types; the man page yields synonyms.

```yaml
entity: Invocation
context: core
attributes:
  command: { type: enum, values: [init, build, sync, clean] }
  dryRun:  { type: bool }
  force:   { type: bool }
  yes:     { type: bool }
  output:  { type: enum, values: [text, json] }
  tty:     { type: bool }
  workspaceWrites: { type: int, range: [0, 100000], derived: count(self.writes where it.inWorkspace) }
```

## Rules

```yaml
id: C1
kind: invariant
when: self.dryRun
assert: self.workspaceWrites = 0
final: true
prose: "--dry-run never writes to the workspace."
---
id: C2
kind: precondition
operation: Invocation.start
when: not (self.dryRun and self.force)
prose: "--dry-run and --force are mutually exclusive."
```

A decision table governs the output format:

```yaml
table: output-format
context: core
inputs:  [self.output, self.tty]
outputs: [self.color, self.progress]
hitPolicy: unique
rows:
  - { output: text, tty: true,  color: true,  progress: true }
  - { output: text, tty: false, color: false, progress: false }
  - { output: json, tty: true,  color: false, progress: false }
```

The checker warns that the table is incomplete: `output = json, tty = false` matches no row.

## The contradiction

A plugin maintainer writes: "`sync --dry-run` refreshes the remote index cache so the preview is accurate." The translator emits an effect on `Workspace.write` for `sync` that applies even when `dryRun` is true.

The checker reports:

> `sync --dry-run` writes 1 file (the index cache), violating C1. C1 is `final`.

## Resolution

The maintainers decide the cache lives outside the workspace, in a declared temporary location that C1 does not cover. C1 already counts only writes where `it.inWorkspace` holds, so the plugin declares its cache writes with `inWorkspace = false`, and the checker passes. A core maintainer still approves, because the plugin changes what `inWorkspace` means for its writes. Generated tests (pytest with Hypothesis) run `sync` with every combination of `dryRun`, `force` and `yes`.

## Assurance levels

| Rule | Level |
| --- | --- |
| C1, C2, the output table | Proved |
| "`build` on a 10k-file project finishes in under 5 s" | Verified (`measured`, benchmark in CI) |
| "Error messages suggest a fix" | Reviewed (`informal`) |
