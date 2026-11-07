# 26-11-07-598 — Each console binds commands the way its platform does

- **Status**: Accepted
- **Date**: 2026-11-07
- Accepted by the owner by merging #844, 2026-11-07
- **Issue**: [#786](https://github.com/discobox-ai/discobox/issues/786) (epic), [#787](https://github.com/discobox-ai/discobox/issues/787)

## Context

The TUI and the desktop app must do the same things and stay in step
([26-11-07-876](26-11-07-876-the-console-is-one-core-with-two-renderers.md)).
A terminal program and a desktop app are operated differently, though, and
copying the TUI's keys onto the desktop would make an app that feels like a
terminal.

Checked at `00541f9c90fefeabe1b61134a737fab287861803`:

- The TUI's window commands sit behind a leader (`cli/internal/keys`,
  Ctrl-A by default), because every other key belongs to the program in the
  pane.
  - Workspace keys are string literals in `pane.go`'s `paneOptions` and
    `tree.go`.
  - [26-11-06-671](26-11-06-671-the-console-palette-composes-commands-and-boxes.md)
    §4 puts commands in one registry with bindings, and requires a coverage
    test so that no binding escapes it.
  - The only leaderless chords are Ctrl-Shift-K/J, because a classic terminal
    cannot encode Ctrl-Shift at all.
- A desktop app receives modifiers directly. On macOS ⌘ never reaches a
  terminal program. On Windows and Linux Ctrl does, which is why Windows
  Terminal, GNOME Terminal and VS Code's terminal put their own commands on
  Ctrl+Shift.
- The prototype ran one scenario through both consoles: a shell, maximize,
  restore, another shell. The TUI pressed leader keys and the app pressed its
  own shortcuts. Both passed.

## Decision

### 1. Parity is per command, not per key

Every command in the core's registry is reachable in both consoles, each in
the way its platform expects. A command one console lacks is either a bug or
an entry in the parity ledger (`docs/console/parity.md`) with its reason.
Names, states, glyphs, confirmations and eligibility reasons are the same
everywhere, because they come from the core.

### 2. Bindings are data, per frontend

Each registry entry carries bindings keyed by frontend: `tui`, `desktop`. A
desktop binding is written with `primary`:
- on macOS `primary` is ⌘;
- on Windows and Linux it is Ctrl+Shift.

A desktop binding never adds Shift to `primary`, which off macOS would be the
same chord. The desktop has no leader. Its commands are also reached through:
- the menu bar on macOS, or an in-window equivalent elsewhere;
- context menus;
- the palette;
- the pointer.

A key no binding takes goes to the focused terminal.

### 3. The TUI is reworked onto the same registry

- The TUI's bindings move out of literals into the registry. 671's coverage
  test extends to every frontend's bindings.
- The palette is the main way in.
- Leaderless chords are added wherever a terminal can deliver them
  unambiguously.
- The leader remains for everything else.

Each rework lands with the parity scenario that pins it.

### 4. The keyboard comes first

A new interaction gets a command, and a binding in each frontend, before
either console adds a gesture for it. The TUI stays a complete client.

### 5. Shared scenarios hold the two together

`cli/internal/console/testdata/parity/*.json` holds scenarios written in
command IDs. Each scenario says what the console shows after each command, in
terms both frontends can answer. Both consoles run every scenario in CI, each
pressing its own bindings. Each frontend also has a test that fails when a
registry command has no binding and no ledger entry.

## Alternatives rejected

- **The same keys everywhere.** The desktop would need a leader, or the TUI
  would need chords a terminal cannot send. Either way one console behaves
  like the other platform's app.
- **The desktop keeps Ctrl alone for its commands on Windows.** Ctrl-T, Ctrl-W
  and Ctrl-K belong to the programs in the panes (readline, editors).
- **Parity by screenshot comparison.** The two consoles differ in layout and
  chrome by design. Comparing pixels would fail on every intended difference,
  and still miss behavioural drift.

## Consequences

- 671's registry gains per-frontend bindings and moves into the core. Its
  coverage test applies to both frontends.
- The prototype's dotted command IDs (`box.stop`) are dropped. The registry's
  existing IDs are the stable ones.
- The help screen, hints, menus and palette in each console are generated
  from the registry.
