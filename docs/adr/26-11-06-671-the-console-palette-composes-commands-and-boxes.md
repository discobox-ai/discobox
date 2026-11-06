# 26-11-06-671 — The console palette composes commands and boxes

- **Status**: Accepted
- **Date**: 2026-11-06
- Accepted by the owner by merging #719, 2026-11-06
- **Issue**: [#717](https://github.com/discobox-ai/discobox/issues/717)

## Context

Finding one of dozens of boxes requires reading and scrolling the console's
list. Commands are discoverable through hints and menus, but not by typing.
The palette needs to find both, in either order, without becoming a second
implementation of each command.

Checked at `3ba0e59a752cc4487a4066d57e466610f85a3a95`:

- `cli/internal/tui/fuzzy.go` supplies smart-case, per-word subsequence matching
  and rune hit positions, but no ranking score. `filter.go` chooses server,
  folder and tags rather than free text.
- `pane.go`'s `paneOptions` and `tree.go`'s `boxStep` already bind leader-k to
  the previous box. Leader-arrow navigation offers the same operation.
- `model.go`'s `actions`, `verbs`, `interactions` and `actOn` describe only part
  of the commands. Global, tree, page and pane commands have other dispatch
  paths. Stop accepts suspended boxes too; settings edits one box. Archive
  does not always ask a question; purge explicitly defaults to No.
- `tree.go`'s `landOn` switches to live or parked boxes and only points at the
  rest; `switchTo` can start a box. `boxlinks.go`'s `focusAddressed` preserves
  the one-box nature of a bare attach/run window.
- `data.go`'s `Listing` already carries all-server snapshots, waiting and
  unreachable servers, and per-server observation times. The list applies
  its view filters afterwards. No search API is needed.

This extends the console without superseding an accepted ADR. The tree's
mouse behavior remains [26-10-31-319](26-10-31-319-a-press-on-a-workspace-tree-row-switches-to-it.md).
The palette's result rows follow [0088](0088-the-launcher-window-answers-the-mouse-and-owns-selection.md)
§4: a press picks a row and a double-click chooses it. Machine-wide saved
filters and drafts remain [26-10-20-596](26-10-20-596-the-console-keeps-one-memory-per-machine-and-the-last-source-leads.md);
the new usage history below is transient.

## Decision

### 1. One modal palette, reached by leader-k or a bare colon

Leader-k opens the palette from the list, workspace tree and workspace panes,
including a page over the panes when the window owns its leader. It replaces
only the previous-box **k alias**, in both pane bindings and window dispatch.
Leader-up/down, leader-j and the existing enhanced-keyboard walk remain.
Hints, help and tests must agree. Leader matching retains the configured
leader's modifier rules.

A bare `:` also opens it when the list or tree has the keys. Neither `:` nor
leader-k interrupts a dialog, run-options card or composer. In the composer,
Ctrl-A remains start-of-line and `:` remains text. Slash retains its search
meaning in audit and dialog bodies. No application receives a bare opener
intercepted from a pane. A clickable palette hint offers the same entry.

The card has the readline input above up to 12 result rows, with scrolling,
a count, scope and key hints. Width is terminal width less a margin; height
is capped by available rows, so input and exit controls fit at 80×24 and on
smaller terminals. The screen behind stays drawn and dimmed. On bare attach,
the window-owned overlay covers the trusted status row too, following the
TUI review rule for modal provenance.

The palette takes keys, paste, hints and mouse ahead of the underlying screen.
Closing restores its focus without replaying the closing event. The leader
is disarmed on opening and closing. Outside clicks dismiss and are consumed.

### 2. Three states make the two orders explicit

The palette has a query and at most one chip: none, a command, or a box.

| State | Results | Enter or double-click | Tab |
| --- | --- | --- | --- |
| No chip | Boxes and commands | A box navigates; a targeted command becomes a chip; a global command opens its existing flow | A box becomes a chip; a targeted command becomes a chip; a global command fills its name without executing |
| Command chip | Boxes evaluated for that command | Dispatch the chosen eligible box | Move to the next result |
| Box chip | Commands evaluated for that box | Dispatch the chosen eligible command | Move to the next result |

With no chip, Tab is **completion**, not next-result navigation. This provides
box-first selection without changing Enter's navigation meaning or requiring
a modified Enter that classic terminals cannot reliably encode. Down/Ctrl-N
and Up/Ctrl-P always move; Shift-Tab always moves back. PgUp/PgDn move by a
visible page. These differences are stated in the card's clickable hints.
Each box row also has a clickable `commands` control, equivalent to Tab on
that row. Clicking elsewhere on a result picks it; double-click chooses it.

Choosing a chip clears the query used to select it. With a command chip,
typing narrows boxes; with a box chip, typing narrows commands. Esc first drops
the chip **keeping the current query**, then clears a nonempty query, then
closes. Clicking the chip or its × performs the same drop. No focus-only
selection, completion or chip change executes a command.

A typed space after an exact, unique, single-word alias of a targeted command
commits that command as the chip. Thus `stop hcp` becomes `stop ▸ hcp`.
Pasting that text produces the same state; the first space is the commit
boundary. Exact aliases are case-insensitive; fuzzy prefixes never commit.
Multiword command names use Tab or Enter on their row. With a chip already
present, spaces belong to its query. Global commands never execute on space
or paste. An ID-shaped first token is never interpreted as a command alias.
The automatic chip is visible and reversible by Esc, as any other chip is.

### 3. Navigation and execution keep the command's existing ownership

Enter on an unchipped box closes the palette and uses `landOn` semantics:
switch to its live/parked workspace, otherwise point at its row without
starting it. In a one-shot attach/run window, a different box instead gets
the existing explanation of how to open another window; this window never
switches to it or uncovers the console list. Its own box remains reachable.

A command chip requests one box, never the list's visual selection. A box
chip offers only commands accepting a box. Global commands such as new box,
credentials, servers and quit are available without a chip and enter their
existing flows immediately on Enter. **Settings remains box-targeted.** A
server-scoped global page preserves its existing server picker/default;
searching all servers does not silently change that scope.

Dispatch closes the palette before entering the existing command flow. It
passes an explicit target, independent of whichever workspace is visible;
commands must not infer their target from `paneBox` when another box was
chosen. In particular attach/shell/tools/issue/files and archive must use
the chosen box and its server, with the same one-shot restrictions as their
ordinary entry points. Existing dialogs, narration, credential consent,
confirmations and destructive defaults stay in force. The palette adds no
blanket confirmation and skips none: archive keeps its current behavior,
and purge retains its explicit confirmation with No as default.

### 4. Command descriptors own bindings, search and eligibility together

The TUI owns one command descriptor registry with stable command IDs, labels,
aliases, detail, target kind (global or one box), context availability,
bare/leader bindings, eligibility with a reason, and dispatch. Commands
already supporting batches retain that through their existing list entry;
the palette supplies a single explicit target. No optional interface or
second palette-specific action switch is introduced.

Key registration, command hints/menus and palette entries consume these same
descriptors. Distinct screen gestures may name the same command ID. This
includes global and workspace commands, not merely the current lifecycle
maps. Low-level editing, cursor motion, selection and terminal forwarding
are gestures, not searchable verbs; their bindings explicitly declare that
classification. A coverage test rejects a command binding without a registry
entry, missing palette metadata or dispatch, and duplicate aliases that would
make space completion ambiguous. Tests also enumerate the window and pane
registrations so a new handwritten binding cannot evade that classification.

Eligibility is the command's existing rule factored into the descriptor,
shared with shortcuts and menus, rather than inferred from a state string in
the palette. Stop therefore offers running **and suspended** boxes; start
uses `startable`; attach accounts for runtime/image availability, including
error states; rename/settings reject archived boxes. Tools and repository
commands use their existing capability checks. Context restrictions such as
a one-shot window are checked as well as target eligibility.

Eligible matches are shown normally. If none exist, matching ineligible
results are shown dimmed with their reasons and cannot execute; the empty
message distinguishes no textual match, no applicable box, and incomplete
server results. On Enter, look the target up again by server and full ID and
re-evaluate eligibility. A disappeared or newly ineligible target leaves the
palette open with a reason, never falls back to a neighboring row. Server
errors after dispatch use the ordinary command error flow.

### 5. Search spans every listed server, separately from the current view

Search uses the unfiltered `Listing` snapshot across all servers the console
already lists. It ignores the header's server, folder and tag filters and
says `All servers · all folders` in the card. It never dials an unknown
server. Waiting, unreachable and stale-server indications remain visible,
using `Listing.Waiting`, `Unreachable` and `Listed`; partial results are useful
immediately and never presented as a complete absence.

Archived boxes are excluded initially. A clickable `Include archived` toggle
and Alt-A change palette scope only, without changing the list's A toggle.
The palette resets this choice when reopened. Choosing unarchive or purge
automatically enables this scope and states that it did so. Merely typing
`archived` does not reveal archived boxes. Navigating to a box outside the
header's filters changes only the in-memory view as needed to reveal it,
with a visible explanation; it never saves over the person's filters. The
implementation must keep this temporary reveal distinct from a deliberate
filter edit and restore it when leaving that navigation, including when
returning from a workspace.

Each box is identified by its server identity and full ID, not its row number
or display name. The data-source boundary supplies the configured server
identity even where the single-server display label is empty. Commands route
through that server's data source. Highlight and chips retain that identity
through live updates; a removed box cannot be replaced by the next result.

Matching reuses `fuzzyQuery`'s smart-case and per-term subsequences. Terms may
match different fields, but subsequences cannot cross field boundaries. Fields
are full/short ID (including the ID without `sbx_`), display/configured name,
description, repository and branch, tag keys/values and `key=value`, harness,
pool, server and state. Rows show kind, status tag, name, short ID, repository
and server; command rows show detail and the binding for the current context.
Matched text is highlighted and all box-written text uses the existing safe
rendering path.

### 6. Ranking is deterministic; history lives only in this window

An empty query orders the current workspace box (otherwise the list cursor's
box) first, then recently visited boxes, then commands by usage count, then
unused boxes in ordinary list order. Every candidate appears once. On a
command chip it applies that box ordering to the eligible candidates; on a
box chip it orders commands by usage.

With text, compare these tiers in order:

1. An ID-shaped query (`sbx_` or a contiguous short/full ID match) puts boxes
   before commands. Otherwise an exact command name/alias puts commands
   first. Otherwise both kinds compete by match quality.
2. Exact ID/name/command/alias, then prefix in those fields, then contiguous
   match there, then scattered match there, then matches requiring detail
   fields. Within a tier, fewer gaps and earlier starts win; the matcher must
   expose ranking evidence without changing existing filter acceptance.
3. Box recency or command usage, then stable label, server identity and ID
   (command ID for commands) break ties.

All query terms must match; a strong hit for one word cannot bypass the
others. ID priority precedes exact-verb priority when both apply. Tests fix
this ordering, including mixed fields and smart-case.

Recency records successful navigation, not cursor movement. Usage counts
record a command entering its existing flow, not highlighting or completing
it; cancelling a follow-up still counts as discovering/using that command.
Both are bounded maps on `Model`, shared across parked workspaces and scoped
to this window: at most 100 recent boxes and one count per registered command.
They vanish when it exits, and are not added to `console.json`. Revisit durable
history only if cross-window ranking is requested; it would need an explicit
merge/pruning policy for concurrent windows and removed servers.

## Alternatives rejected

- **Keep leader-k and choose another palette key.** The requested mnemonic is
  useful from every workspace; previous-box already has leader-up. Removing
  that one alias is smaller than introducing another palette spelling.
- **Tab always moves down.** It leaves box-first selection needing a new,
  less portable chord or makes Enter stop navigating. Completion makes Tab
  the explicit way to compose; the arrows and Ctrl-N/P remain uniform.
- **Infer commands from fuzzy text or execute global commands on space.** Box
  names can resemble verbs. Only an exact targeted alias plus a deliberate
  boundary commits, visibly, and nothing executes until Enter/double-click.
- **Restrict search to the current filters.** A forgotten folder/server filter
  would hide precisely the box somebody is trying to locate. The scope label
  and temporary reveal make all-server search explicit without rewriting
  saved preferences.
- **Separate palette command metadata and eligibility tables.** A command or
  capability would eventually be added to only one. A shared registry and
  registration coverage make that omission detectable.
- **Save usage with the existing machine-wide console memory.** Concurrent
  windows would need merge semantics and a retention policy for convenience
  ranking. Session-only history answers the initial need without that state.

## Consequences and implementation boundaries

The registry refactor crosses list, tree, pane and page dispatch; the palette
cannot be delivered as a search card over the lifecycle map alone. The live
TUI design must change with implementation, not with this proposal.

Implementation still chooses descriptor/type names, the bounded ranking
algorithm within the ordering above, glyphs, truncation budgets and temporary
view-state representation. These are implementation details, not permission
to change the gestures, scope, execution rules or history lifetime above.

Delivery evidence must cover registry/binding completeness, matching and
ranking, both chip orders, typed and pasted completion, Esc steps, eligibility
(including suspended/error/archived), preserved command follow-ups, explicit
cross-server targets, live disappearance, partial/stale listings, temporary
filter reveal and one-shot restrictions. Driver tests use the existing fakes
and `synctest` conventions. Runtime captures at 80×24 and 160×48 must show list,
tree and pane entry, narrow layout, disabled/empty results and mouse controls.
This ADR PR contains no implementation or runtime captures.
