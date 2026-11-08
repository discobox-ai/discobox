# console

The console core (ADR 26-11-07-876): what the console means, shared by every
frontend that draws it. `cli/internal/tui` is one renderer of it; the desktop
app is the other. Each renderer owns layout, focus, keys, pointer and drawing
(876 §2); this package owns the rules behind them.

## Shape

```mermaid
flowchart LR
    CLI["cli.apiDataSource"] -->|implements| DS["DataSource"]
    DS --> Core["console"]
    Core --> TUI["tui (renderer)"]
    Core --> Desk["desktop (renderer, over the bridge)"]
```

- **It imports no frontend**: not `internal/tui`, not `internal/cli`, and
  nothing of Bubble Tea or termpane. A rule a renderer needs is moved here,
  never exported back out of a renderer.
- **Navigation stays the renderer's** (876 §2). Which box is on screen, which
  tab has focus, whether there is a sidebar: a rule that depends on one is
  handed it explicitly by the renderer.

## The data seam

`data.go`. `DataSource` is the boundary between the console and the server,
and its types (`Sandbox`, `Session`, `Listing`, `Verb`, `Exec`, the secret,
credential, audit and issue types) are the console's vocabulary. The CLI
implements it once (`cli.apiDataSource`); every frontend reads through it.

What a box allows is answered from its own fields, here: `Attachable`,
`Startable`, `Repairable`, `Live`, its standing and status tags (`tags.go`),
its folder (`folder.go`), the issue or pull request it is tagged with
(`WorkNumber`). Presentation of those answers — layout, column budgets — is
the renderer's; the glyphs and their colour tokens are the spec's.

## The list

`list.go`, `query.go`. `List` is the box listing as the console shows it
(876 §1); a renderer embeds it and adds the cursor, the marks, scrolling and
drawing. It is a value, so a renderer tries a filter on a copy (the filter
card's counts) without touching the one on screen; a copy shares the listing,
so taking a listing or its marks is the original's to do.

```mermaid
flowchart LR
    L["Listing (SetAll, SetUnreachable, SetWaiting, SetListed)"] --> List
    F["Filter: server, folder, tags, archived"] --> List
    List --> R["Rows: sections, then trees"]
    List --> N["Nesting: depth, last, rules"]
    List --> T["Notes: servers not answering, still listing, behind"]
    List --> S["Search: every server, no filter"]
```

- **Order is the core's.** `Rows` is the filtered rows as drawn: on every
  folder (`Grouped`), sections in `Folders` order — the window's own first —
  each newest-first as the listing has them; inside one, each box followed by
  the boxes it created (ADR 0111 §3). A box is drawn under the nearest
  creator the filter shows, wherever it is filed itself, and only there.
- **Nesting is structure, not glyphs.** `Nesting` says how deep a row is,
  whether it is its creator's last, and where a branch carries on above it;
  the renderer draws the branch (`├ └ │` in the TUI).
- **The filter is a value** (`Filter`): server, folder, tags (one value per
  key, `AddTag`) and the archived. `Revealing` is the view that shows one
  box whatever the person's filter hid (671 §5); a renderer holds it in
  place of theirs and puts theirs back, never saving it. `Filter.View` is
  the filter as saved (`Session.View`) and `Session.Restore` the filter a
  saved view opens on.
- **Row text is the core's.** `Sandbox.Remark` is what a row says in place
  of its name when picked out — naming the command that answers it, never a
  key, which is each frontend's binding — `StateWord` its state as a word,
  `List.Origin` where it came from (nothing on a nested row); `Notes` and
  `ListedAgo` are what the list says about servers. Glyphs, colours and
  column budgets are the renderer's.
- **An archive is listed from the press** (`MarkArchiving`) until a listing
  sent after its answer lands (`SettleArchives`), counted in listings sent.
- **Search ignores the filter** (`Search`, 671 §5): every listed server,
  archived only when asked for, matched by `Query` (fzf-style, smart case)
  within one field at a time (`MatchFields`), ranked by `Rank`; `Partial` and
  `Stale` are what it says about servers it has no or old rows from. Ranking
  boxes among commands is the palette's.

## The command registry

`commands.go` (ADR 26-11-06-671 §4, moved here by 876 §1). One `Command` per
thing the console does, by a stable ID: its label, detail and aliases, its
target (the window, or one box — a batch for the box's lifecycle commands),
and its bindings keyed by frontend (ADR 26-11-07-598 §2): `TUI` (bare keys,
the leader sequence, the menu path) and `Desktop` (chords written with
`primary`).

```mermaid
flowchart LR
    K["a key, a menu row, the palette, a click"] -->|command ID + explicit targets| D["Dispatch"]
    D --> E["entry: Context + targets → enabled, why, unavailable"]
    E -->|refused| F1["Frontend.Unavailable / Cannot / OfferUpgrade"]
    E -->|runs| U["Registry.Use"] --> F2["Frontend.<Command>"]
```

- **Eligibility is the registry's.** `Entries`/`Entry` answer whether a
  command can act, and why not, from the targets' own fields and the
  `Context` the frontend hands it: workspace, pane, the list's keys, tree,
  the box whose workspace is open, the one box an attach window belongs to,
  and the credential inbox's answers for a box. A frontend never rewrites an
  entry's answer.
  `Lifecycle` is the box's action menu with its reasons.
- **Dispatch is against the core** (`Dispatch`): it resolves the command,
  refuses before entering a flow (nothing selected, a server gone, a
  context that rules it out, a lifecycle command its targets cannot take, a
  box whose image is gone), counts the use, then calls the one `Frontend`
  method that command enters. A target is the one given, never what is on
  screen; a window's command takes none. A box command's other
  preconditions (a tag to open, a request waiting) are refused by its flow,
  in its own words; `Entry` reports them so a frontend offers only what will
  act.
- **`Frontend` has a method per command.** A command is added as a registry
  entry, a case in `Dispatch` and a `Frontend` method together: the compiler
  holds every frontend to the method, and `TestEveryCommandDispatches` holds
  the registry to `Dispatch`. A frontend that cannot offer a command yet says
  so, and the parity ledger (`docs/console/parity.md`) says why. The flows
  behind the methods — dialogs, workspaces, pages — are the frontend's.
- **Use is counted once, as a flow is entered.** A command whose flow is
  also entered by gestures outside the registry (a page's row in the tree)
  counts itself there (`countedByFlow`); `Use`/`Usage` are the window's
  palette ranking (671 §6), and live as long as the `Registry` does.
- **The registry is tested here** for completeness (described, unambiguous
  aliases and keys, primary without shift, every command dispatching to a
  flow of its own); each frontend tests that its own bindings have entries.

## The spec

`spec.go` (876 §4). `NewSpec` is what every frontend draws the same way, as
data: the registry's commands with each frontend's bindings, the state and
agent glyphs (`StateGlyph`, `AgentGlyph`) by colour token, and every colour
token in both palettes, read from the design language's tokens
(`assets/brand`, 363 §4). `discobox desktop` hands it to the app on `hello`,
and `task generate` writes it to `desktop/src/spec.json`; the TUI draws the
same glyphs by each token's xterm index. Nothing in it is read out of a
renderer. Lists on the wire are never null.

- **The parity scenarios are here** (`testdata/parity`, 598 §5), written in
  registry IDs; `TestParityScenariosNameRegistryCommands` holds each step to
  a command both consoles bind from a pane. The TUI and the desktop app run
  them, each pressing its own binding.
