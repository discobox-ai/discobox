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

- **The bridge** is ADR 26-11-07-289's, protocol 5. Every registry command the
  app answers goes to the core as a `command` (with the app's navigation as
  its context), and the app enters its own flows when the result says `enter`;
  the listing, a workspace's sessions and its header are subscriptions; terminals are
  execstream frames on streams the app allocates. Its client and wire types
  are the `discobox-bridge` crate (`bridge/`), with nothing of the window in
  it; the types (`bridge/src/wire.rs`) are generated from the schema that is
  generated from the bridge's Go types, and none is written by hand.
- **The list** is the core's (`console.List`, over the listing topic): its
  rows in order, folder sections, each box under the one that made it (drawn
  as strokes), the remark a picked row says in place of its name, the server
  notes, and the filter with its choices. The app holds the filter and
  subscribes again to change it: the filter control at the list's head (and
  `servers`, `primary+f`) opens the core's choices with their counts, and `archived`
  shows or hides the archived. Selection is the app's: Space marks a row,
  Shift-Space draws a range the cursor extends, `primary+d` lets them go; ⌘-click (Ctrl-click elsewhere)
  marks, Shift-click marks a run. A box's lifecycle verbs, from its row's
  menu or the palette, act on every box marked, in one batch the core rules on;
  the marks go once it has acted.
- **The workspace** is the core's sessions in two columns (`Workspace` in
  `main.rs`), drawn as framed columns with their tab chips in the frame's edge:
  - the divider between the columns drags where they split (each keeps a
    fifth of the width), and a double press puts them back half and half;
  - a chip drags along its strip to reorder its column, closes its session
    (or dismisses a held tab) from its close control, and maximizes its
    column on a double press;
  - switching to another box parks the workspace, as the core's `Park` does:
    its streams stay attached and its screens keep emulating, but it polls
    nothing and its ports are no longer forwarded, until it is back on
    screen. One whose primary never arrived is let go instead, and `detach`
    lets the workspace on screen go, as the TUI's does;
  - what a session's ending does is the core's (`Tab.End`, on the stream's
    `ended` message): the primary takes the workspace with it, a broken
    stream or a tool takes its tab and says why, and a program that finished
    is held with its last screen, a faint dot on its chip and what it says
    over its screen; a gold dot and a gold note while its stream reconnects;
  - the header is the core's (`console.Header`, over the header topic, whose
    subscription is the workspace's port forward): the box's id, the issue and
    pull request it is tagged with, its git position and word in their tone,
    its diff, its desktop and the ports it serves — a forwarded web port is a
    link that opens the browser — and the rest of its tags.
- **The spec** is the console core's (`cli/internal/console/spec.go`): the
  registry's commands by their IDs (`attach`, `shell`) with each frontend's
  bindings, the glyphs, and the design language's colour tokens in both
  palettes. Every shortcut and every list key is the spec's: a registry
  command's, moving between panes and boxes included, or a gesture's (the
  list's cursor, ⌘1–9 for a tab). The menus and the palette keep their own
  keys while they are open. Where focus goes is the app's own (ADR 26-11-07-876 §2); which key
  moves it is the registry's, and `no_shortcut_is_written_outside_the_spec`
  fails on one matched by hand. A registry command the app does not answer
  (`ANSWERED` in `main.rs`) says so, and has an entry in the parity ledger.
- **The sheets** are the core's forms and questions (`sheet.rs`), drawn as a
  modal panel over the window, which takes no key or press until it is
  answered (on macOS the native menu's commands wait for it too): rename,
  settings and purge's confirmation so far.
  - A form is the core's fields, each opening on its value as the core reads
    it: typed, a picker of its choices, or dimmed with why it takes no answer
    (and stepped over); a masked value is never drawn. The app holds what is
    typed and chosen, sends a draft as an answer changes (the form comes back
    with what applies recomputed), and submits only the fields the person
    changed, so an untouched field is never an edit; the core's refusal is
    said under the field it names, with what was typed kept. Tab and the
    arrows move between fields, Left, Right and Space turn a picker, Enter
    submits, Esc cancels.
  - A question is a yes or no, or a pick of one by key. A destruction
    (`defaultNo`, purge's) opens on No and draws its yes in the error colour,
    so Enter never purges. Esc answers a yes or no no, as the core says
    leaving one is, and declines a pick of one.
  - Sheets queue in the order they arrive; each goes when its command's result
    lands, answered or not.
- **The command surfaces** (ADR 26-11-07-598 §2) reach every registry command:
  - the palette (`primary+k`) is ADR 26-11-06-671's: a query and at most one
    chip, in either order. A box command's alias and a space make it the
    chip (`stop rea…`), and the boxes it can act on follow; Tab makes a box
    the chip (or its row's `commands`), and the commands it can take follow.
    Enter with a chip runs the command on that box, never on what is on
    screen; Enter on a box goes to it, showing one the filter hides through
    a view that is never saved, until the window leaves it. Esc drops the
    chip, then the query, then the palette. What it finds, in what order, is
    the core's (the palette topic); the boxes this window went to rank an
    empty query.
  - the menu bar, off macOS drawn in the window (F10 from the list): the
    registry's commands by their desktop `menu`, each entry as the core
    rules on it where it was opened (the commands topic), disabled with why;
    a pane's menu (its secondary press) is the `Session` menu.
  - a box's actions menu, from its row, its header's control or
    `shift+f10`;
  - the help (F1): every command by its menu, with its shortcut;
  - tooltips: each control's command's detail and shortcut, from the spec.

  On macOS the native menu bar is still the hand list in `main.rs` (#908).
- **The look** is ADR 26-11-07-363's (`theme.rs`, `icons.rs`, `mark.rs`):
  - JetBrains Mono throughout, chrome and terminals, bundled from `fonts/` under
    the SIL Open Font License (`fonts/OFL.txt`);
  - the spec's tokens: the colours in both palettes, following the system's
    appearance, and the space, radii and strokes; terminal content keeps the
    program's colours, on the palette's sunken ground;
  - the TUI's devices drawn natively: rounded vector frames, tab chips in a
    frame's top edge, icon buttons with tooltips, keycaps, rounded selections
    with hover and press, and the mark as vector art from
    `assets/brand/logo-purple.svg`;
  - spring motion for focus, chips, the sidebar and maximize, and none in
    terminal content;
  - a status line for messages only: shortcuts are on tooltips, keycaps in the
    sidebar and menus, and the palette's shortcut column.

  Not yet: a user's own choice of theme over the system's (§3; Rune's
  `--theme` picks one only for a headless run); `LANGUAGE.md`'s calm spring for
  a theme change (Rune gives an appearance change no spring of its own, so it
  springs on each element's).

  `identity_tests` fails, in the chrome's sources, on a box-drawing character, a
  block element or a bracket typed around a label; on a colour or length the
  theme names that is not a token in the spec; and on a colour written out
  anywhere but the theme (only transparent, and a floating panel's black
  shadow, are not tokens).
- **The CLI** runs the app named with `--binary` or `DISCOBOX_DESKTOP_BINARY`.
  Otherwise, on Linux, a release CLI downloads the app its desktop manifest
  pins into `<discobox state>/desktop/<digest>/`, and a development CLI runs the
  one beside it. On macOS and Windows it says the platform is not yet supported
  (ADR 26-11-07-983 §4).
- **The release** builds the app for Linux amd64 and arm64
  (`go tool task release:desktop`, in `nix develop .#desktop`): cross-linked by
  zig through cargo-zigbuild against glibc `DESKTOP_GLIBC` (Taskfile), and
  packed as `discobox-desktop-linux-<arch>.tar.gz`, the executable alone.
  Nothing it links but glibc: Wayland, X11, xkbcommon and Vulkan are loaded at
  run time. `discobox desktop --print-manifest` shows what a CLI pins.

| File | What |
| --- | --- |
| `src/main.rs` | State: boxes, workspaces (parked or live), panes, focus; commands, their results and the flows they enter; `run_command` |
| `bridge/src/wire.rs` | The wire's types, generated (`go tool task generate`) from `cli/internal/console/bridge/wire.schema.json`, itself generated from the bridge's Go types: every message, the resize payload, the protocol and spec versions, the frame kinds. Never edited by hand |
| `bridge/src/lib.rs` | The bridge client: frames over any pipe, the opening `hello` and its version checks, a reader handing on what arrives, and its way out for a message the types cannot read |
| `bridge/tests/contract.rs` | The bridge's contract suite: this client against the CLI's real bridge serving a fake core (`TestContractCore` in `cli/internal/console/bridge`), one process per test |
| `src/backend.rs` | The bridge client on stdin/stdout, its messages as the window's events, and a mock that answers the same messages with local shells, for running without a CLI |
| `src/view.rs` | The list, the sidebar, the workspace (its header, columns, split and tab chips), frames |
| `src/overlay.rs` | The palette, the menu bar and its menus, a box's actions menu, a pane's menu, the help, and the list's filter menu |
| `src/sheet.rs` | The core's forms and questions as sheets: the queue, the answers held while they are typed, drafts, refusals, and their keys |
| `src/keys.rs` | Desktop shortcuts from the spec (`primary` is ⌘ on macOS, Ctrl+Shift elsewhere), the app's navigation shortcuts; keys as xterm bytes |
| `src/term.rs` | A pane's terminal (`alacritty_terminal`) and its screen as rows |
| `src/theme.rs` | The spec's tokens (colours in both palettes, and the appearance that picks one; space, radii and strokes), the bundled faces and type, and springs |
| `src/icons.rs`, `src/mark.rs` | The window's vector icons, and the mark from `assets/brand/logo-purple.svg` |
| `fonts/` | JetBrains Mono 2.304 at the weights the chrome sets and the italic, and its licence |
| `src/parity_tests.rs` | Runs `cli/internal/console/testdata/parity` with the desktop's keys |
| `src/workspace_tests.rs` | Parking and unparking, what an ending does, chips dragged along their strip, the split, the header's links |
| `src/command_tests.rs` | Commands that must not reach past what they name; every registry command answered or in the ledger |
| `src/sheet_tests.rs` | Rename, settings and purge through their sheets on the mock: what applies, a refusal, Enter on a destruction, a pick of one, a sheet taking every key |
| `src/surface_tests.rs` | Every command in a menu and found by the palette; the palette's two orders, Esc and reveal; the menu bar's keys; menus that say why not |
| `src/identity_tests.rs` | No TUI character art in the chrome, every colour and length a token, no colour of the chrome's own, the bundled face measured |

## Build, run, test

```sh
go tool task build:desktop          # build/discobox-desktop, beside build/discobox
build/discobox desktop              # against the configured server
go tool task desktop:spec           # refresh src/spec.json, which the mock runs with (generate does too)
go tool task desktop:test           # unit, parity and contract tests
go tool task desktop:snap           # every scene headless in both themes, to build/desktop/<scene>-<theme>.png
cd desktop && cargo run --release  # the app on the mock, without a CLI
```

- **Debugging:** `DISCOBOX_DESKTOP_DEBUG=1` logs every bridge message each way
  to stderr.
- **Headless:** `--snap ws.png --scene workspace` renders a frame with no
  window; `--theme light` (or `dark`) stands in for the system's appearance.
- **Toolchain:** `nix develop .#desktop` is the repository's shell plus rustup
  and, on Linux, the libraries the app builds and runs with. Without a GPU,
  render on Mesa's software driver it carries:
  `VK_ICD_FILENAMES="$DISCOBOX_LAVAPIPE_ICD"`. Outside Nix, Linux builds need
  `libxkbcommon-dev` and `libwayland-dev`, and `mesa-vulkan-drivers` with
  `VK_ICD_FILENAMES=/usr/share/vulkan/icd.d/lvp_icd.json` for no GPU.
- **CI:** the `desktop` job, on Linux only, runs `go tool task ci:desktop` in
  that shell: `desktop:test`, then `desktop:snap`, every scene rendered
  headless in both themes on the software driver and kept as the `desktop-scenes` artifact.
  macOS and Windows do not build the app, or run the bridge's contract
  suite, in CI yet. The Go side, its parity scenarios included, runs in
  every job.
- **Parity:** a command one console lacks is listed, with its reason, in
  [`docs/console/parity.md`](../docs/console/parity.md).
