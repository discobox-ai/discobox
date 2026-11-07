# desktop: `discobox desktop`

The discobox console as a native app, in Rust on
[Rune](https://github.com/anomalyco/rune). Rune is pinned to a `dev` revision
in `Cargo.toml`.

`discobox desktop` runs this app as a child process with `--bridge`, and serves
it the console over stdin and stdout (`cli/internal/cli/desktop.go`). The app
holds no credentials and talks to no server.

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

- **The bridge** serves `DataSource` calls rather than the console core (until
  #798). Its messages are `hello`, `list`, `execs`, `open`, `newShell`,
  `newTerminal`, `endExec`, `actions` and `do`, plus a pushed `listing`, with
  terminal streams as execstream frames.
- **The spec** (`cli/internal/tui/spec.go`) is exported from the TUI rather
  than owned by a core (until #797). Its command IDs are the prototype's
  dotted ones (`box.attach`, `pane.newShell`), here, in the parity scenarios
  and in this app; ADR 26-11-07-598 keeps the core registry's (`attach`), and
  they change with it.
- **The look** uses Inter and the system's monospace face, and draws some of the
  TUI's characters (until #803).
- **The CLI** finds the app with `--binary`, then `DISCOBOX_DESKTOP_BINARY`, then
  beside itself, then on PATH. Downloading on demand comes with #801.

| File | What |
| --- | --- |
| `src/main.rs` | State: boxes, workspaces (parked or live), panes, focus; requests and their answers; `run_command` |
| `src/backend.rs` | The bridge client (frames on stdin/stdout), and a mock with local shells for running without a CLI |
| `src/view.rs` | The list, the sidebar, the workspace, frames and tabs |
| `src/overlay.rs` | The palette and a box's actions menu |
| `src/keys.rs` | Desktop shortcuts from the spec (`primary` is ⌘ on macOS, Ctrl+Shift elsewhere); keys as xterm bytes |
| `src/term.rs` | A pane's terminal (`alacritty_terminal`) and its screen as rows |
| `src/logo.rs`, `src/theme.rs` | The mark from the TUI's `cli/internal/tui/logo.json`, and colours from the spec's tokens |
| `src/parity_tests.rs` | Runs `cli/internal/tui/testdata/parity` with the desktop's keys |

## Build, run, test

```sh
go tool task build:desktop          # build/discobox-desktop, beside build/discobox
build/discobox desktop              # against the configured server
go tool task desktop:spec           # refresh src/spec.json, which the mock runs with (generate does too)
cd desktop && cargo test --release  # unit and parity tests, on the mock
cargo run --release                 # the app on the mock, without a CLI
```

- **Debugging:** `DISCOBOX_DESKTOP_DEBUG=1` logs every bridge request and answer
  to stderr.
- **Headless:** `--snap ws.png --scene workspace` renders a frame with no
  window.
- **CI:** does not build or test the app yet (#788); the Go side, its parity
  scenario included, runs in every job.
- **Linux:** builds need `libxkbcommon-dev` and `libwayland-dev`. Without a GPU,
  use `mesa-vulkan-drivers` with
  `VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/lvp_icd.json`.
