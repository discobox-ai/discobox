# desktop: `discobox desktop`

The discobox console as a native app, in Rust on
[Rune](https://github.com/anomalyco/rune). Rune is pinned to a `dev` revision
in `Cargo.toml`.

`discobox desktop` runs this app as a child process with `--bridge`, and serves
it the console core over stdin and stdout
([`cli/internal/console/bridge`](../cli/internal/console/bridge/DESIGN.md)).
The app holds no credentials and talks to no server.

The design is in the accepted ADRs:
- [26-11-07-876](../docs/adr/26-11-07-876-the-console-is-one-core-with-two-renderers.md): one core, two renderers;
- [26-11-07-983](../docs/adr/26-11-07-983-discobox-desktop-downloads-its-app-and-serves-it-over-stdio.md): delivery and the process;
- [26-11-07-289](../docs/adr/26-11-07-289-the-desktop-bridge-carries-the-console-core.md): the bridge;
- [26-11-07-598](../docs/adr/26-11-07-598-each-console-binds-commands-the-way-its-platform-does.md): bindings and parity;
- [26-11-07-363](../docs/adr/26-11-07-363-the-desktop-keeps-the-console-identity-drawn-natively.md): identity.

The work is tracked in epic #786.

## What is here today

This is the prototype the ADRs were tested with. It is the starting point for
the epic, and does not yet follow the ADRs everywhere:

- **The bridge** is ADR 26-11-07-289's, protocol 2. Every registry command the
  app answers goes to the core as a `command` (with the app's navigation as
  its context), and the app enters its own flows when the result says `enter`;
  the listing and a workspace's sessions are subscriptions; terminals are
  execstream frames on streams the app allocates. The app draws no question or
  form yet (#808): it declines each and says so. Its client and wire types
  are the `discobox-bridge` crate (`bridge/`), with nothing of the window in
  it; the types (`bridge/src/wire.rs`) are generated from the schema that is
  generated from the bridge's Go types, and none is written by hand.
- **The list** is the boxes as the server lists them, which the app orders,
  groups and nests itself (`view::list_order`), until the listing carries the
  core's list (#804).
- **The spec** is the console core's (`cli/internal/console/spec.go`): the
  registry's commands by their IDs (`attach`, `shell`) with each frontend's
  bindings, the glyphs, and the design language's colour tokens in both
  palettes. The app draws the dark one (both, following the system, with
  #803). Every shortcut and every list key is the spec's: a registry
  command's, moving between panes and boxes included, or a gesture's (the
  list's cursor, ⌘1–9 for a tab). The menus and the palette keep their own
  keys while they are open. Where focus goes is the app's own (ADR 26-11-07-876 §2); which key
  moves it is the registry's, and `no_shortcut_is_written_outside_the_spec`
  fails on one matched by hand. A registry command the app does not answer
  (`ANSWERED` in `main.rs`) says so, and has an entry in the parity ledger.
- **The look** uses Inter and the system's monospace face, and draws some of the
  TUI's characters (until #803).
- **The CLI** finds the app with `--binary`, then `DISCOBOX_DESKTOP_BINARY`, then
  beside itself, then on PATH. Downloading on demand comes with #801.

| File | What |
| --- | --- |
| `src/main.rs` | State: boxes, workspaces (parked or live), panes, focus; commands, their results and the flows they enter; `run_command` |
| `bridge/src/wire.rs` | The wire's types, generated (`go tool task generate`) from `cli/internal/console/bridge/wire.schema.json`, itself generated from the bridge's Go types: every message, the resize payload, the protocol and spec versions, the frame kinds. Never edited by hand |
| `bridge/src/lib.rs` | The bridge client: frames over any pipe, the opening `hello` and its version checks, a reader handing on what arrives, and its way out for a message the types cannot read |
| `bridge/tests/contract.rs` | The bridge's contract suite: this client against the CLI's real bridge serving a fake core (`TestContractCore` in `cli/internal/console/bridge`), one process per test |
| `src/backend.rs` | The bridge client on stdin/stdout, its messages as the window's events, and a mock that answers the same messages with local shells, for running without a CLI |
| `src/view.rs` | The list, the sidebar, the workspace, frames and tabs |
| `src/overlay.rs` | The palette and a box's actions menu |
| `src/keys.rs` | Desktop shortcuts from the spec (`primary` is ⌘ on macOS, Ctrl+Shift elsewhere), the app's navigation shortcuts; keys as xterm bytes |
| `src/term.rs` | A pane's terminal (`alacritty_terminal`) and its screen as rows |
| `src/logo.rs`, `src/theme.rs` | The mark from the TUI's `cli/internal/tui/logo.json`, and colours from the spec's tokens |
| `src/parity_tests.rs` | Runs `cli/internal/console/testdata/parity` with the desktop's keys |
| `src/command_tests.rs` | Commands that must not reach past what they name; every registry command answered or in the ledger |

## Build, run, test

```sh
go tool task build:desktop          # build/discobox-desktop, beside build/discobox
build/discobox desktop              # against the configured server
go tool task desktop:spec           # refresh src/spec.json, which the mock runs with (generate does too)
go tool task desktop:test           # unit, parity and contract tests
go tool task desktop:bridge         # the bridge's contract suite alone: Rust and Go, no window
go tool task desktop:snap           # every scene headless, to build/desktop/<scene>.png
cd desktop && cargo run --release  # the app on the mock, without a CLI
```

- **Debugging:** `DISCOBOX_DESKTOP_DEBUG=1` logs every bridge message each way
  to stderr.
- **Headless:** `--snap ws.png --scene workspace` renders a frame with no
  window.
- **Toolchain:** `nix develop .#desktop` is the repository's shell plus rustup
  and, on Linux, the libraries the app builds and runs with. Without a GPU,
  render on Mesa's software driver it carries:
  `VK_ICD_FILENAMES="$DISCOBOX_LAVAPIPE_ICD"`. Outside Nix, Linux builds need
  `libxkbcommon-dev` and `libwayland-dev`, and `mesa-vulkan-drivers` with
  `VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/lvp_icd.json` for no GPU.
- **CI:** the `desktop` job, on Linux only, runs `go tool task ci:desktop` in
  that shell: `desktop:test`, then `desktop:snap`, every scene rendered
  headless on the software driver and kept as the `desktop-scenes` artifact.
  macOS and Windows do not build the app in CI yet; their
  `desktop-bridge-*` jobs run the bridge's contract suite
  (`desktop:bridge`). The Go side, its parity scenarios included, runs in
  every job.
- **Parity:** a command one console lacks is listed, with its reason, in
  [`docs/console/parity.md`](../docs/console/parity.md).
