# 26-11-04-028 — A person's harness files are their own layer, and a bundle refresh never writes it

- **Status**: Accepted (§6 amended before implementation landed: the first seed splits every built-in, and a write to an unsplit set is refused)
- **Date**: 2026-11-04

  Accepted by the owner by merging #550, 2026-11-04.
- **Relates to**:
  [0002](0002-harness-config-is-the-only-harness-concept.md), which split a
  config's files into the bundle's `Files` baseline and the configure flow's
  `ConfiguredFiles`. This adds a third set beside them and changes neither.
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md) §1, whose
  runtime layer's `Files` overlay by path carries the new set to a sandbox
  with no change to the merge.
  [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md)
  §3, whose save-back patches `ConfiguredFiles` by path. §5 here narrows
  which set a saved path goes to.

## Context

Release validation found (#524, from #515) that a file a person adds to a
built-in harness is lost, on v0.17.0 and v0.18.0 alike:

- `discobox admin harnesses update claude-code --file P=V` sends `files`, and
  `UpdateHarnessConfig` (`server/internal/resources/harnessconfigs/service.go`)
  replaces `HarnessConfig.Files` with it wholesale. The built-in
  `.claude.json` (a `createOnly` template) and `.claude/settings.json` go.
- `HarnessConfig.Files` is documented as the bundle's baseline
  (`server/internal/model/model.go`), and `snapshotImageMetadata` rewrites it
  from the bundle's label. `SeedBuiltIns` runs that on every server start for
  any built-in whose bundle digest or manifest digest moved, which is every
  upgrade, and `RefreshHarnessConfigImage` runs it on demand. The person's
  file goes, and the bundle's come back.
- The same holds for a user-registered harness on `refresh-image`. And
  `CreateHarnessConfig` drops the `files` of its body without a word.
- `harnesses edit` and the console's `f` edit a bundle file in place, in
  `Files` (`cli/internal/cli/harness_edit.go`), so that edit is lost the same
  way.

So no file set today belongs to a person:

| Set | Written by | Overwritten by |
| --- | --- | --- |
| `Files` | the bundle's label, and `update --file` | every re-snapshot (seed, refresh) |
| `ConfiguredFiles` | the configure flow, save-back, `harnesses edit` | every configure (the output is authoritative, ADR 0009), and deconfigure clears it |

## Decision

**A harness config gains a third file set, `FileOverrides`, which belongs to
the person who wrote it. A file in it adds a path or replaces the one beneath
it, and only a person changes it. Seeding, refresh, configure and
deconfigure never write it. `files` stays the bundle's, and no request
writes it.**

### 1. `FileOverrides` is a person's, per path

`model.HarnessConfig` gains `FileOverrides []HarnessConfigFile`
(column `file_overrides`, JSON, as `files` and `configured_files` are). Each
entry is one path, with its content and its `createOnly`, exactly the shape
of the other two sets.

- `snapshotImageMetadata`, `SeedBuiltIns` and `RefreshHarnessConfigImage`
  never read or write it. A bundle that moves leaves it as it was.
- Configure doesn't write it, and deconfigure doesn't clear it: neither
  wrote it. A person's file is still there when the harness is configured
  again.
- Deleting the config deletes it with the row.

### 2. By path, `FileOverrides` wins over `ConfiguredFiles`, which wins over `Files`

The effective file at a path is the override's, else the configured file's,
else the bundle's. The person's word is the last one about that path.

The set travels on its own, as the two today do: `sandbox.ResolvedHarnessConfig`
and the pool wire's `ResolvedHarnessConfig` (`pool-agent/api/openapi/pool.yaml`)
each gain a `fileOverrides` field. `pool-agent/sandboxruntime` writes
`Document.Runtime.Files` as the configured files followed by the overrides.
`sandboxconfig.mergeFiles` already upserts in order, so the later entry wins
at a shared path. `sandboxconfig` doesn't change, and a test in it pins that
a later runtime entry wins.

A config-mode sandbox (the configure flow's) is handed the overrides too: it
runs the harness the way every sandbox will.

### 3. The API writes `fileOverrides`, never `files`

- `HarnessConfig` (the response) keeps `files` and `configuredFiles`, and
  adds `fileOverrides`.
- `UpdateHarnessConfigBody` replaces `files` with `fileOverrides`, which
  replaces the set, the same way `configuredFiles` does, under the
  `revision` the caller read.
- `CreateHarnessConfigBody` replaces `files` with `fileOverrides`, which is
  the new config's first set, so `create --file` stops being ignored.
- A body still naming `files` is refused (400) by the schema
  (`additionalProperties: false`). The CLI is released with its server, so no
  old client of this field has to be kept working.

### 4. The CLI adds and replaces one path at a time

- `harnesses update H --file P=V` reads the config, sets `P` in its
  overrides (replacing an override already at `P`), and writes the set back
  under the revision it read. Every other path stays. `--create-only-file P`
  marks it `createOnly`, as it already does.
- `--remove-file P` (new, repeatable) removes the override at `P`, which shows
  the configured or bundle file beneath it again. `--clear-file` removes every
  override, and nothing else.
- `harnesses create --file` writes the first overrides.
- `harnesses edit H P` and the console's `f` edit the effective file. A
  configured file is edited in `ConfiguredFiles`, as today. A bundle file or
  an override is written as an override, so the edit survives the bundle
  moving.
- `harnesses get` and the console's harnesses screen list every path once,
  with the set its effective file comes from (bundle, configured, override),
  and name a file the override shadows.

### 5. Save-back writes to the set that holds the path

A save-back change (ADR 26-10-26-573 §3) to a path that an override holds
changes the override. Any other path patches `ConfiguredFiles`, as today.
Without this, a saved change would land beneath the person's override and
never reach a sandbox. The capture baseline (`store.CaptureBaseline`) and
the preview's `previous` (`describeCapturePreview`) describe the effective
file, with the overrides on top.

### 6. A config upgraded from before this has its added files moved, once

The migration adds `file_overrides` as NULL, meaning "not yet split". The
first re-snapshot of a config whose set is NULL (`SeedBuiltIns` or
`RefreshHarnessConfigImage`, the writes that would lose the files) moves
every `Files` entry whose path the freshly read bundle doesn't declare into
`FileOverrides`, before writing the bundle's files. Every write after that
leaves the set non-NULL, even when it's empty, so the split runs once.

*Amended before implementation landed:* `SeedBuiltIns` re-snapshots a
built-in whose set is NULL even when its digests haven't moved. It inspects
every built-in on every pass anyway, so the split is judged against the
bundle the config is on, and no built-in is left waiting on a bundle that
may not move for a while. A request that writes `fileOverrides` on a config
still NULL (one registered by hand and not refreshed yet, or a built-in whose
bundle couldn't be read) is refused with 409, naming the refresh, or for a
config registered before bundles the bundle, that splits it. The original
text left such a config's files in `Files` until its bundle moved. Writing
the set in the meantime would have ended the split, so a person's earlier
files would have been lost at the next move. Splitting inline on that write
was rejected: it made a file edit depend on reaching the registry, read the
tag rather than the pinned digest, and refused configs with no bundle.

What this can't recover: an edit to a bundle path made before this release.
Its content and a bundle that changed that file look the same. A file the old
bundle declared and the new one dropped is moved as though a person had
added it. It's visible as an override and removable with `--remove-file`.

### 7. Removing a bundle's file is deferred

An override replaces a path. It doesn't hide a path the bundle ships. Nobody
has asked to hide one, and one that's done wrong (a bundle relying on its
own file) breaks the harness quietly. Revisit when someone needs a built-in
without one of its files: a tombstone entry (`remove: true`) in
`FileOverrides` is the shape to start from.

## Alternatives rejected

- **Merge in the CLI and carry undeclared paths across a re-snapshot.** It
  would mean `--file` merging client-side, with `snapshotImageMetadata`
  keeping `Files` entries the new bundle doesn't declare. It needs no new
  field, but nothing marks which entries are the person's. A person's
  override of a bundle path still goes on upgrade, which is the issue's own
  "Done when" for `.claude/settings.json`. A file the bundle dropped is kept
  forever. §6 accepts that ambiguity once, for data written before the
  distinction existed, and never again.
- **Keep a person's files in `ConfiguredFiles`.** Save-back already patches
  it. But configure's output is authoritative (ADR 0009): a configure command
  that writes only its own files drops the person's, and deconfigure clears
  them. Merging configure's output by path instead would break configure's
  ability to remove a file it wrote before, and make deconfigure guess which
  files were its own.
- **Refuse `files` on a built-in.** It's the issue's fallback, and it's
  honest, but it takes away the only way to give a built-in harness a file
  short of registering a copy of it, which then loses the bundle tracking that
  built-ins exist for.
- **Snapshot the bundle's files separately, and store `Files` as the merged
  result.** It's the same new column under another name, and every reader of
  `Files` would have to tell the bundle's from the person's again.

## Consequences

- `harnesses update claude-code --file P=V`, then a restart or an upgrade,
  leaves `.claude.json` (still `createOnly`), `.claude/settings.json` and `P`
  in `harnesses get claude-code`. That's #524's "Done when".
- The request bodies' `files` field is gone. A script that sets it gets a 400
  naming the field, and moves to `fileOverrides` or the CLI.
- A configure command that writes a file at a path a person overrode is
  shadowed by the override. `harnesses get` says so (§4). Removing the
  override shows the configured file.
- Three sets, not two, reach the pool. `pool-agent` and the server change
  together, as they already do for `configuredFiles`.
