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

- **The bridge** is ADR 26-11-07-289's, protocol 13. Every registry command the
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
  `servers`, `primary+l`) opens the core's choices with their counts, and `archived`
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
  - the box's services are tabs at the head of the terminals column while the
    core says they are worth one (the sessions topic): a running one's output
    after its history, one not running its card and last output, with its
    state said quietly over it. They are read-only, stopped rather than ended
    (no close on their chips), and numbered apart (`primary+alt+1`–`9`), so
    one starting never renumbers a tab; a tab whose service's run moved opens
    again, and one the listing drops goes. The services menu (`services`,
    `primary+alt+s`) lists every declared service; each opens its own menu:
    its output (its tab, while there is one), then start, stop and restart as
    the core offers them;
  - the header is the core's (`console.Header`, over the header topic, whose
    subscription is the workspace's port forward): the box's id, the issue and
    pull request it is tagged with, its git position and word in their tone,
    its diff, its desktop and the ports it serves — a forwarded web port is a
    link that opens the browser — and the rest of its tags.
- **A pane is a terminal** (`term.rs`), with termpane's behaviours
  (`docs/console/parity.md`, "Terminal behaviours"): a drag selects (a word on
  a double click, a line on a triple, a block with Alt), copied on release;
  `primary+c` copies a selection and `primary+v` pastes, bracketed when the
  program asks; a program's OSC 52 copy reaches the clipboard and its reads
  get nothing; a link it made (OSC 8), or a URL in its text, opens on
  ⌘-click (Ctrl-click elsewhere) where the core says it goes (the bridge's
  `follow`): a URL the box printed at the port its header's forward bound, and
  a box's address (the spec's `linkSchemes`, drawn as a link and lit under
  the pointer) on a plain left click too, to that box, unless the mouse was
  taken back to select with; a program that asked for the mouse hears it in its encoding
  until `mouse` takes it back or Shift is held; the wheel scrolls the
  program, a pager's arrows or the scrollback; `primary+f` finds through the
  scrollback and `shift+pageup`/`pagedown` page it; wide characters and emoji
  are drawn at their cells; an input method composes at the cursor; and
  `repaint` asserts the pane's size again and asks for its screen. The workspace
  header leads with what the box's agent is doing, as the core's header says
  it (`console.Header.Agent`): the core's choice between what the primary's
  program reports (OSC 7501) and the listing's poll. A program's copy reaches the clipboard
  only from the box on screen.
- **Images are carried in** (`paste.rs`), as the TUI carries them
  (`cli/internal/tui/imagepaste.go`): a box cannot read this machine's
  clipboard or files, so an image meant for one of its sessions is written
  into it by the core (`upload`) and the path it landed at is pasted in its
  place, which every agent takes as an image. An image means: the paste
  gesture with an image and no text on the clipboard, or files copied there;
  `paste image` (Ctrl+V, Alt+V) on a harness terminal with an image or image
  files there — with none, the key goes to the program, and shells never take
  it; and files dropped on a pane, lit while the drag is over it, which the
  CLI carries in when every one is an image and which are otherwise pasted as
  their paths, quoted for a shell. Which panes take images and the image keys
  is the core's (`Tab.images`, `Tab.image_keys`). What is typed or pasted
  meanwhile is held and sent after the paths, never the mouse or the wheel;
  one image at a time per pane. A clipboard image past what a bridge
  message carries is said, never sent. On Linux, drops reach the window under
  X11 only: winit reports no file drags on Wayland.
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
  settings, purge's confirmation, a harness's reset and variant, a file
  open in the person's editor, and a secret stored, edited, bound, deleted
  or unbound.
  - A form is what the core says it is about, then its fields, each opening on its value as the core reads
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
    lands, answered or not, and a submitted form goes too once its command goes
    on (asks, fills another form or reports a step), as a create's card gives
    way to the questions after it.
- **The harnesses page** (`harnesses.rs`) is the core's: the harnesses of
  the server the list is narrowed to, drawn beside the list in place of the
  workspace, each with its default, kind, secrets, files, age and bundle. Its
  keys are its own while it is up: the arrows move, Enter reads a harness's
  config card, Shift+F10 (or the context-menu key, or a secondary press) opens
  its actions as the core offers them, Esc leaves. Each action is the
  harnesses command naming the harness: the default, a reset the core asks
  about first, a variant through the core's form, a file picked from the
  harness's files and edited. A box taking the screen closes the page.
  - **A file is edited in the person's editor**: the core sends `edit` with
    a copy of the file under a text format's extension (the bridge's), the
    app opens it in a text editor (`open -t` on macOS, Notepad on Windows,
    `xdg-open` on Linux, by the type the extension names; never a shell or
    a name a box chose) and puts up a sheet that waits: Done answers `saved`
    and what was saved is written back; Esc gives it up, and the file is as
    it was.
  - **A box's harness changes** (`review config`, from its menu, the palette
    or the Box menu) are a panel: where the candidate stands, what saving
    does, each file's diff, the credentials and the diagnostics, and the
    core's offers, each saying why when it cannot act. It opens on reading
    it again, never on saving. Saving or discarding names the candidate the
    panel showed.
- **A box's tools** (`tools.rs`) are the core's (the tools command):
  - the tools menu (`tools`, `primary+alt+o`, the Box menu or the palette)
    lists the box's tools, each with what it is or why it cannot run, then its
    ssh command and git URL to copy, then the files of each tool that carries
    any;
  - a tool that runs in the box is a session whose tab is after the shells,
    given the window as it opens (the workspace maximized onto it):
    `maximize` gives the workspace back with the tool running on its tab, and
    `end` ends it. Choosing a tool that is running shows its tab, never a
    second session; one a window left running is picked up with the box's
    sessions;
  - a tool that runs on this machine (an IDE) is handed the box, and the
    status line says so;
  - a tool's file is edited in the person's editor as a harness's is, and
    saving says it is what the next box to open the tool gets.
- **The secrets page** (`secrets.rs`) is the core's, drawn as the harnesses
  page is, and one page is up at a time: the secrets of the server the list
  is narrowed to, each with where it may go, what stands on it (or that it
  is refused) and its age. The arrows move, Enter reads a secret, Shift+F10
  opens the page's actions (open, bind, edit, history, delete, a new
  secret, read again), Esc leaves; `refresh` reads it again too.
  - Reading a secret is a panel: the core's card, then "Bind this secret…"
    and its live bindings; Enter reads a binding in full (or binds), Delete
    takes one back, asking first as the core asks, No the quick answer.
  - A new secret, an edit and a binding are the core's forms as sheets, each
    with what the core says above it (why a refused secret's edit was
    opened, what a binding is); a delete asks first. The page, and a panel on it, follow the secrets read
    again after each.
  - A secret's binding history is a panel of its decisions, each read in
    full, and the delegation it was handed on under when that still stands.
- **Creating a box** is the core's (`console.RunOptions`, held by the bridge):
  - the composer, under the list, is what the new box is given to do: Return
    creates and Shift+Return is a new line; under it, what a create would be
    made with (the composer topic), lit where somebody chose it, and the run
    options. Its prompt is told to the CLI once it has been still a moment and
    on the way out, and the next window opens on it;
  - `new` (`primary+n`) from the composer opens the create card as a sheet —
    where it is cut from, or a source typed, the harness, the pool and server
    where there is a choice, skills and other sources, rows that grow as they
    are filled, and the command it would run — then asks what the core asks
    before a create (a default harness, uncommitted changes, copying a
    directory), says its steps on the status line, takes the prompt out of the
    composer once the server has the box, and opens its workspace, which says
    what the box is being made to do (the provisioning topic). From a
    workspace, `new` goes back to the composer first;
  - `options` is every run option as a sheet; what it keeps is what the next
    create is made with;
  - a folder dropped on the composer, which lights while the drag carries one,
    is where the next create is cut from: `options` with the folder as its
    source, checked by the core as a source typed on the card is, and named on
    the composer's line.
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
| `src/sheet.rs` | The core's forms, questions and edits as sheets: the queue, the answers held while they are typed, drafts, refusals, a file opened in the person's editor, and their keys |
| `src/tools.rs` | The tools menu, a tool's tab given the window, a tool on this machine handed the box, a tool's files edited, an address copied |
| `src/harnesses.rs` | The harnesses page, a harness's actions, file picker and config card, and a box's harness changes reviewed |
| `src/paste.rs` | Images pasted or dropped on a pane carried into its box, the pane's input held meanwhile; a folder dropped on the composer as the next source |
| `src/secrets.rs` | The secrets page, its actions, and a secret, a binding, the history and a decision read as panels |
| `src/keys.rs` | Desktop shortcuts from the spec (`primary` is ⌘ on macOS, Ctrl+Shift elsewhere), the app's navigation shortcuts; keys as xterm bytes |
| `src/term.rs` | A pane's terminal (`alacritty_terminal`): its screen as rows, selection, the program's copies and links, mouse reporting, the wheel, paste, find, wide characters and the input method's composition |
| `src/theme.rs` | The spec's tokens (colours in both palettes, and the appearance that picks one; space, radii and strokes), the bundled faces and type, and springs |
| `src/icons.rs`, `src/mark.rs` | The window's vector icons, and the mark from `assets/brand/logo-purple.svg` |
| `fonts/` | JetBrains Mono 2.304 at the weights the chrome sets and the italic, and its licence |
| `src/parity_tests.rs` | Runs `cli/internal/console/testdata/parity` with the desktop's keys |
| `src/service_tests.rs` | Services as tabs in the core's order, numbered apart, closed and reopened by the listing, stopped not ended; the services menu and its verbs |
| `src/create_tests.rs` | The composer, the create card and the run options through their sheets on the mock: a source refused, rows that grow, a detached create, `new` from a workspace, the draft |
| `src/paste_tests.rs` | An image pasted, under the image key, or dropped, carried in with what was typed held behind it; a failure, a drop that is not images, one at a time, one too large; a folder dropped on the composer |
| `src/workspace_tests.rs` | Parking and unparking, what an ending does, chips dragged along their strip, the split, the header's links |
| `src/command_tests.rs` | Commands that must not reach past what they name; every registry command answered or in the ledger |
| `src/tool_tests.rs` | The tools menu as the core lists it, a tool given the window and put away, one running shown not started again, a host tool, a file edited and given up, an address copied |
| `src/secret_tests.rs` | The page's secrets and keys, a secret and a binding read, an unbind asked first, a binding, an edit, a delete and a new secret through their sheets, the history, refresh |
| `src/harness_tests.rs` | The page's harnesses and keys, actions as the core offers them, the card, a reset asked first, a file edited and given up, a variant's form, harness changes reviewed and saved |
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
