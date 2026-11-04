# 26-11-04-711 — A reset keeps a person's harness files, and a save never writes them

- **Status**: Accepted (supersedes [26-11-04-028](26-11-04-028-a-persons-harness-files-are-their-own-layer-and-a-bundle-refresh-never-writes-it.md)
  §5 and §1's "configure and deconfigure never write it", and narrows
  [26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md)
  §3's reset)
- **Date**: 2026-11-04
- **Relates to**:
  [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md)
  §3, whose save-back patches `ConfiguredFiles` by path; this restores that
  for every path.

## Context

ADR 26-11-04-028 gave a harness config a third file set, `FileOverrides`,
which belongs to the person who wrote it. It was written while configure
still existed: §1 says configure never writes the set and deconfigure never
clears it, and §5 has a save-back change at a path an override holds patch
the override, so the saved change reaches sandboxes.

ADR 26-11-02-152 removes configure. Its reset (§3) takes a config back to its
bundle's defaults: it deletes what boxes saved, `ConfiguredFiles` and the
logins save-back created. The two ADRs landed side by side, so neither said
what reset does to a person's files, and together they make one thing worse.
A box's saved change at an overridden path becomes the person's file under
028 §5. A hook or MCP server a box slipped into an overridden
`.claude/settings.json` then runs in every box on the harness. If reset keeps
a person's files, a plain reset — the way to undo a bad save — no longer
takes it back; if reset clears them, the person loses files nobody made in a
box, the loss 028 exists to stop.

## Decision

### 1. A plain reset keeps a person's files; asked, it clears them too

Reset (`admin harness reset`, `POST .../reset`, the harnesses screen's `r`)
deletes what boxes saved and keeps `FileOverrides`. A person's file is a
choice nobody made in a box, so taking a box's changes back does not take it.

`admin harness reset --include-overrides` (`includeOverrides` on the API)
clears `FileOverrides` as well, for a config with nothing but its bundle's
files. Cleared, the set is empty, never NULL, which would mark a config as not
yet split (028 §6). The console asks, when the harness has any of a person's
files: "Reset, keep your files" first, or "Reset your files too".

### 2. A save never writes a person's file

A save-back change patches `ConfiguredFiles` at every path. At a path an
override holds, the saved file sits beneath the override and does not reach a
sandbox while the override stands. The preview marks such a change
`shadowed` (`harnesscapture.FileChange.Shadowed`, set from the stored
overrides, never from what a sandbox sends), and the CLI and the console's
review say "shadowed by your override, which boxes keep getting". Removing or
editing the override is how a shadowed save takes effect.

So a bad save is always in the saved set, where a plain reset takes it back,
and a person's file changes only when the person changes it.

## Alternatives rejected

- **Keep 028 §5: a save writes the override.** The save takes effect at once,
  but a box's file becomes the person's and outlives a plain reset, which is
  the one undo a person has for a save.
- **Track which overrides a save last wrote, and revert them on reset.** It
  keeps the save effective and the reset honest, at the cost of per-path
  provenance in the stored set, for the same result as not writing the
  override at all.
- **Refuse a save at an overridden path.** Nothing silent and nothing
  inert, but some changes could not be saved, and the person would have to
  edit their own file to take one.
- **Reset clears every set but the bundle's.** "Back to the bundle's
  defaults" read literally, and the bug 028 fixed — a person's file lost to
  something nobody asked to remove it — comes back through reset.

## Consequences

- A saved change at a path a person overrode is kept but shadowed; the review
  and `harnesses get` (028 §4) say so.
- A plain reset leaves a person's files in place; `--include-overrides` is the
  way to a config with nothing but its bundle.
- Main's implementation of 028 §5, which wrote the override, never shipped:
  no stored config holds a box's save as a person's file.
