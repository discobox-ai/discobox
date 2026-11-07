# 26-11-07-876 — The console is one core with two renderers

- **Status**: Accepted
- **Date**: 2026-11-07
- Accepted by the owner by merging #844, 2026-11-07
- **Issue**: [#786](https://github.com/discobox-ai/discobox/issues/786) (epic), [#787](https://github.com/discobox-ai/discobox/issues/787)

## Context

`discobox desktop` is to be a native app with the console's layout and
behaviour. It will be drawn by a Rust program on Rune, alongside
`discobox console`, the TUI in `cli/internal/tui`. Both must keep doing the
same things as each changes, while each interacts the way its platform
expects ([26-11-07-598](26-11-07-598-each-console-binds-commands-the-way-its-platform-does.md)).

Checked at `00541f9c90fefeabe1b61134a737fab287861803`:

- `cli/internal/tui` is about 35,200 non-comment lines with 31,900 lines of
  tests. Most of it is behaviour rather than drawing:
  - `model.go`'s `actions` decides what a box allows and why;
  - `list.go` and `tree.go` decide order, sections, nesting and row text;
  - `workspace.go`'s `terminalExec` and `execBefore` decide which sessions a
    workspace opens and where;
  - `dialog.go` and `form.go` decide what each question asks and how it
    validates;
  - `secrets.go`, `credentials.go` and their neighbours hold about 3,600
    lines of approval rules.
- `commands.go`'s `consoleCommands` is the registry that
  [26-11-06-671](26-11-06-671-the-console-palette-composes-commands-and-boxes.md)
  §4 required. Its descriptors carry IDs, labels, aliases, keys, eligibility
  with a reason, and dispatch. Dispatch is a closure over `*Model`, so no
  other frontend can run a command.
- `DataSource` (`data.go`) has 74 methods. It is the boundary between the
  window and the server, and is implemented by the CLI's `apiDataSource`.
- A prototype (`console-rune`, `cli/internal/cli/desktop.go`) served
  `DataSource` methods to a Rust app. To show the actions menu it had to
  export `(*Model).actions`, the state glyph functions and the theme
  constants. It also copied `workspace.go`'s rule for which sessions open.
  Flows that need a question or a form (rename, settings, purge's confirm,
  create, credentials) had no way across.

## Decision

### 1. A console core package owns what the console means

A new package, `cli/internal/console`, owns the console's domain state and
rules:

- the box listing as the console shows it: order, sections, nesting, row text
  and filtering;
- the command registry of 671 §4, moved out of `tui`: IDs, labels, aliases,
  targets, eligibility with a reason, confirmations and dispatch, with
  dispatch against the core rather than `*Model`;
- workspaces: which sessions a box's workspace holds and in which column,
  their labels, parking, polling, services and the push loop;
- forms and dialogs as descriptions (fields, choices, defaults, validation,
  submit), and the questions a command asks while it runs (credential
  confirms, purge's confirmation);
- narration: progress, warnings and status that a command reports.

The core depends on `DataSource`. It depends on no frontend.

### 2. Each renderer owns how the console is operated

`cli/internal/tui` and the desktop app each own:

- layout and breakpoints;
- focus, selection, cursors and scrolling;
- key and pointer handling, which ends in a command ID;
- drawing, including terminal emulation (termpane in the TUI,
  `alacritty_terminal` on desktop);
- platform affordances such as menus, notifications, the clipboard and drag
  and drop.

Navigation is the renderer's. Which box is on screen, which tab has focus, and
whether the sidebar is shown are frontend state. They become core state only
when a rule depends on them, and then the renderer passes them explicitly. One
example is a command targeting the box on screen.

### 3. The TUI moves onto the core first, without changing behaviour

The TUI is refactored onto the core before the desktop grows past the
prototype. The existing tests are the check. A test that asserts an internal
moved to the core is rewritten to assert it there; none is deleted for
convenience.

### 4. The core is the spec

The prototype's `tui.Spec` exporter is replaced by the core's own data:
- commands with their per-frontend bindings ([598](26-11-07-598-each-console-binds-commands-the-way-its-platform-does.md));
- state and agent glyphs with their colour tokens;
- the colour tokens themselves.

Nothing is read back out of a renderer.

## Alternatives rejected

- **The desktop reimplements the console in Rust.** The bridge would stay a
  `DataSource` API. Every rule, from eligibility and its reasons to ordering
  and session placement, would exist twice. It would drift from the first
  release, and the Go tests would protect only one copy.
- **The core owns navigation too, and sends finished screens.** This gives
  the most parity. But every focus move, scroll and hover would round-trip
  through the bridge, and the desktop could not use the platform's own focus,
  menus and pointer behaviour.
- **Keep the logic in `tui` and export what the desktop needs.** The
  prototype did this. Every desktop feature reached into a renderer's
  internals, and the reach-ins were copies in all but name.

## Consequences

- The refactor in §3 crosses most of `cli/internal/tui` and is the largest
  single piece of the port. It lands on the desktop feature branch with the
  TUI's suite green at each step.
- `DESIGN.md` for `cli/internal/tui` changes with the code: the window becomes
  a renderer of `cli/internal/console`, which gets its own `DESIGN.md`.
- Adding a console feature means a core change and a change in each renderer.
  The cost is presentation work only, and the parity rules in 598 make a
  missing renderer visible.
