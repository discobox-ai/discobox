# 0036 — Termpane selection is a mouse-only cell-space overlay

- **Status**: Accepted
- **Date**: 2026-08-12

## Context

Termpane draws a live terminal but has no way to select text from it: its
`DESIGN.md` closes with "scrollback can be read but not selected from". When a
pane enables mouse reporting the host mirrors the mouse to the application and
the user loses native terminal selection with nothing in its place; when the
mouse is off, clicks do nothing at all. Users expect what every terminal
emulator gives them: drag to select, double-click for a word, triple-click for
a line, and a copy that lands on the clipboard — including selections that
span soft-wrapped lines and the scrollback.

A survey of the ecosystem (August 2026) found no reusable component. The two
real implementations — Crush and TUIOS — both live in application `internal/`
packages, coupled to their own screen models; Crush is additionally
FSL-licensed. Nothing packages the gesture handling and the extraction corner
cases (wide runes, wrap joins, block select) behind an importable seam.
`bubbletea#162` has asked for framework-level selection for years. The
clipboard half is solved: Bubble Tea v2 ships OSC 52 as `tea.SetClipboard`.

Two facts about the substrate shape the design. `vt.Emulator` already *is* a
cell grid — `CellAt`, `ScrollbackCellAt`, `Draw(uv.Screen, area)` — so the
screen-buffer-coordinate approach Crush had to build scaffolding for comes
free. And two pieces are missing upstream: neither `x/vt` nor ultraviolet
records a per-line soft-wrap bit (the emulator's "phantom" pending-wrap state
is transient cursor state, dropped at the moment the wrap happens), and the
scrollback exposes no monotonic pushed-lines counter, so cap eviction is
undetectable from outside.

## Decision

Selection is a new component in the termpane module, layered over the pane. It
is mouse-only, active only when the application has not claimed the mouse,
composited in cell space, and anchored in scrollback-absolute coordinates.

### 1. Mouse-only; there is no copy mode

The component handles pointer gestures: press-drag-release, double-click word,
triple-click line, a modifier for block (rectangular) selection, and the
delayed-click disambiguation that keeps a single-click action from firing
inside the double-click window. There is no keyboard-driven selection, no
movable copy-mode cursor, no vim motions, no search. Keyboard selection
belongs to the applications running in the pane and to the user's terminal
emulator; duplicating it here is the largest part of tmux's copy-mode state
machine, bought for a redundant feature.

### 2. Routing: the application's mouse wins, unless the host seizes it

Three states, decided by `MouseMode()` plus one host-owned flag:

- Application mouse **off** (shells, most CLIs): mouse events drive selection.
- Application mouse **on** (vim, htop): events forward via `SendMouse` as
  today; selection is inert. Any nonzero mouse mode counts as **on** —
  stealing motion events an application subscribed to only button events for
  breaks its drag behavior in ways no user can diagnose.
- Host **seize** armed: forwarding is suppressed regardless of what the
  application asked for, and selection gets the events. What arms it — a key,
  a held modifier — is host policy, as is drawing an indicator; the component
  only exposes the flag, because "why is vim ignoring my clicks" must be
  answerable from the chrome.

### 3. Selection is composited in cell space, not spliced into strings

The selection range is applied as a style transform on the emulator's cells,
and the affected rows are serialized from those cells. The transform defaults
to reverse video — legible over anything the application painted — and is
host-replaceable (`func(uv.Style) uv.Style`) for themed highlights. Wide runes
are a primary cell plus a continuation cell, so endpoint snapping and
highlighting both halves fall out of the grid; and the copied text is read
from the same cells that were highlighted, so rendering and extraction cannot
disagree.

`View` keeps two paths: with no active selection, today's `emu.Render()` path,
unchanged and costing nothing; with a selection live, rows are serialized from
cells with the transform applied. The host contract — exact-width rows, styles
closed — is identical on both paths.

### 4. Anchors are scrollback-absolute; a broken anchor clears the selection

An anchor is `(scrollbackTotal + screenRow, column)` at mouse-down. Under the
common case — output scrolling text upward while the user selects or is
scrolled back — the pushed row increments the scrollback length as the content
moves up one row, so the absolute coordinate is untouched and the highlight
rides with the text. Selections spanning the scrollback/screen boundary are no
special case.

The coordinate breaks in three ways, and each clears the selection rather than
letting it slide silently:

- **In-place mutation**: a touched screen row (via the emulator's damage
  tracking, `Touched()`) intersecting the selection means the selected text no
  longer exists.
- **Resize, alt-screen switch, clear**: columns are meaningless after a resize
  (`x/vt` does not reflow), and the alt screen has no scrollback.
- **Cap eviction**: once the scrollback is full every push shifts every index.
  With the upstream counter (§5) the anchor is `Total()`-relative and eviction
  costs nothing; until then, any push while `Len() == MaxLines()` clears the
  selection — visibly honest where a slid highlight is silently wrong.

### 5. Wrap joins and eviction detection come from upstream `x/vt`; heuristics until then

Extraction joins soft-wrapped rows without inserting newlines and trims
trailing pad spaces from each hard line. The wrap bit does not exist upstream,
so it is added there: a per-row wrapped flag on `vt.Screen`, set where the
phantom-wrap linefeed fires, carried through `Scrollback.Push`, exposed per
line — together with a monotonic `Total()` pushed-lines counter on
`Scrollback` (§4). The same fork-then-PR path already used for
`ibuildthecloud/x/ansi` applies.

Until the flag lands, a heuristic ships behind the same interface: scrollback
`Push` trims trailing empty cells, so a stored line at full width reads as
wrapped (on screen: last cell non-empty). Its one failure — a hard-newline
line exactly terminal-width wide joins a line it should not — is accepted
temporarily rather than blocking on upstream.

Wrap bits describe the width the line was written at; without reflow, a
resized pane's old content keeps its old wrap points, as in tmux without
reflow.

### 6. The component selects; the host copies

Completing a gesture yields the extracted text as a message to the host, which
routes it to `tea.SetClipboard` (OSC 52) or wherever else it wants — the
component never touches the clipboard itself, keeping it free of transport and
environment concerns (tmux passthrough, WSL fallbacks) that are host policy.
Block selections extract as the rectangle's rows joined with newlines, each
trimmed of trailing pad.

## Alternatives rejected

**A keyboard copy mode (tmux/TUIOS style).** Rejected as out of scope, not
deferred: cursor movement, visual modes, and search triple the state machine
to replicate what terminal applications and emulators already do. Selection
over scrollback still works without it — `Scroll` already redraws history rows
in place, so drag-plus-autoscroll covers reaching old output.

**Splicing highlight SGRs into rendered row strings.** Post-processing
`Render()` output means parsing to a visual column while tracking every open
style, restoring it at the selection edge, and re-deriving wide-rune
boundaries — reconstructing the cell grid from its serialization when the grid
is one call away. It also makes highlight and extraction two computations that
can drift apart.

**Rendering into a separate `uv.ScreenBuffer` (Crush's shape).** Crush renders
its chat list into a buffer *in order to* have cells to select from. Termpane's
emulator already owns the authoritative cells, including scrollback; a second
buffer is a copy with no new information.

**Selecting whenever the host wants, splitting mouse events with the
application.** Forwarding some events while selection consumes others (e.g.
keeping clicks, stealing drags) breaks applications in undebuggable ways.
Mouse-on is binary; the only override is the explicit seize, which suppresses
forwarding entirely while armed.

**Anchoring selections to viewport rows.** Simpler, but new output makes the
highlight slide off the selected text onto whatever scrolled under it — the
tmux annoyance this design exists to avoid.

**Waiting for upstream before shipping.** The full-width heuristic is correct
on everything except exactly-full hard lines, and the eviction fallback
degrades to clearing a selection during sustained full-buffer output. Both are
rare enough to ship behind the final interface while the `x/vt` PR is in
flight.

## Consequences

- Termpane grows its first gesture-state machine (click counting, drag,
  delayed-click timers) and a second `View` serialization path; the fast path
  and the host row contract are unchanged.
- The mouse-mirroring section of termpane's `DESIGN.md` gains the seize
  override, and "Not here" loses its selection entry when this lands.
- A fork of `charmbracelet/x/vt` joins the existing `x/ansi` fork until the
  wrap-bit and counter PR merges upstream.
- The component is scoped to grids that expose cells, wrap bits, and stable
  line coordinates — an interface the emulator satisfies today and other
  cell-buffer widgets can satisfy later; nothing in it depends on disco.
