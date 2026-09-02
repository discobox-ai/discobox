# 0088 — The launcher window answers the mouse everywhere, and owns selection where it does

- **Status**: Accepted
- **Date**: 2026-09-02

## Context

The `discobox tui` window reports the mouse only while a pane is up
(`paneMouseMode`). On the workspace that bargain is already struck: the
terminal's native selection is traded for the panes' own, clicks focus a pane
or pick a tab, and a press no pane claims drives a selection over the composed
frame (`chrome.go`, ADR 0036).

Every other screen the window draws — the launcher's discobox list, the run
options, the harnesses screen, the secrets screen, the dialogs — is left to the
terminal. A click there does nothing at all. That is the odd half of a window
whose whole subject is a list of things you act on: the pointer is over the row
you mean, and the only way to say so is to walk the cursor to it with the
arrows.

Two facts shape what can be done about it.

**Mouse reporting is per-frame and all-or-nothing.** A terminal either reports
the mouse or it does not; there is no asking for clicks while leaving
drag-select native. So a screen that answers clicks is a screen whose native
selection is gone, and one that must offer selection of its own in its place.

**The window has no widget tree.** Every screen is composed by rendering
strings and joining them, so nothing on screen knows where it landed. The two
controls the workspace already answers presses on — the tab labels and the tool
window's buttons — solve it two different ways: the tab strip records each
label's span as it draws it (`Model.tabSpans`), and the buttons recompute their
position from the layout arithmetic (`buttonAt`).

## Decision

The window reports the mouse on every frame it draws, answers presses against a
hit map recorded while the frame is drawn, and provides the selection it took
away.

### 1. The mouse is reported for the whole window, always

`View.MouseMode` is `MouseModeCellMotion` on every screen, not only while panes
are up. All-motion is still requested only when a focused application asked for
it and the mouse has not been seized.

The alternative — reporting the mouse only on the screens with something to
click — was rejected. It makes drag-select mean the terminal's selection on one
screen and the window's on the next, with no visible difference between them
and no way to know which you are about to get; and it makes Tab into the
harnesses screen silently change what the mouse does. A window whose pointer
behaves differently depending on which screen it is on is one you have to think
about before touching, which is the thing a mouse is for avoiding.

Shift-drag remains the terminal's own escape hatch to a native selection, as it
is in tmux and every full-screen program; the help says so.

### 2. Selection outside a pane is the chrome's, on every screen

`chrome.go`'s selection over the composed frame is not extended, only reached
from further: it already reads the frame back into cells and knows nothing
about which screen produced it. Its rules stand — one selection on screen at a
time, a selection whose text no longer reads back identically is cleared rather
than left highlighting whatever moved under it, and copies go through
`copyText`.

The composer is the exception. It is a field you type into, and a selection
there has to be one that typing replaces, that Backspace deletes, and that
survives the caret moving. That is the textarea's own selection
(`charm.land/bubbles/v2` v2.2.1: `PositionAt`, `BeginSelection`,
`ExtendSelection`, `SelectedText`), so the dependency is raised rather than the
behavior reimplemented over a widget that would keep editing underneath it.

### 3. What a press means is recorded as the frame is drawn, never recomputed

A frame records the controls it drew into a hit map — `zones`: a rectangle, and
what the cell means — and a press is a lookup in it. Positions are marked by
the code that draws the thing, from the same numbers it drew with, at an origin
the composing code pushes as it places each block.

Recomputing positions from the layout was rejected on the evidence already in
this package: the column header was computed separately from its rows and drifted
out of line with them whenever a column dropped on a narrowing window, which is
why `tailColumns` exists to budget both from one arithmetic. A hit map computed
a second time is the same bug with no pixels to show it — a click that lands on
the wrong row is invisible until someone reports it.

The map is the previous frame's, which is exactly the frame the press was
aimed at. `parseChrome` already reads `Model.lastFrame` on the same reasoning.

### 4. A press on a row picks it; the second press acts

A left press on a row moves the cursor to it, focuses its list, and then
continues into the chrome's selection, so the text of a row stays
drag-selectable. A double-click acts on it — attach, the same as Enter, which
is the convention of every list of things you open. The row's name is still
copyable by dragging across it, and double-click keeps its word-select meaning
everywhere that is not a row.

The right button opens the row's actions menu — the one `.` opens — unless a
selection is showing, in which case it copies and clears it as it does in the
panes and the chrome today. The copy gesture wins because it is the more
destructive to lose: a right-click meant for the menu costs one more press,
where a right-click that discards a finished selection costs the selection.

The wheel moves the list cursor, which is what scrolls it: the viewport follows
the cursor (`clamp`), so a scroll offset of its own would be snapped back by
the next refresh. It does not move focus — the wheel scrolls what is under the
pointer, as it does over a pane — so scrolling the list while typing a prompt
leaves the keys in the prompt.

### 5. A hint that names a key is a button for that key

Key hints — the header's `F1 help`, the status line's `a attach`, the folder
line's `Esc prompt` — are answered by handling the press as that key press,
through the same handler the keyboard reaches. So `hints()` returns the key and
the label as a pair rather than a formatted string, and the renderer both joins
them and records where each landed.

The alternative, a mouse handler per hint, was rejected: it is the same action
written twice, and the copy that is wrong is the one nobody presses.

### 6. The middle button pastes the last selection

With mouse reporting on, the terminal no longer pastes on the middle button —
the event comes here instead. The window pastes what X11 would: its own last
selection, which is what the middle button pastes everywhere it works. The
clipboard is not read; nothing here reads the OS clipboard, and Ctrl-V/⌘V are
the terminal's own shortcut and still arrive as `tea.PasteMsg`.

## Alternatives rejected

**Reporting the mouse only on the screens that answer clicks.** See §1: two
selections with no way to tell them apart, changing under you as you move
between screens.

**A retained-mode widget tree.** The window would gain positions for free, and
every screen in it would be rewritten to get them. The hit map buys the same
answer for the code that already draws.

**Keyboard-driven selection (a copy mode).** Rejected for the same reason ADR
0036 rejected it in the panes: it is the largest part of a multiplexer's state
machine, bought for something the terminal and its applications already do.

**Reimplementing selection over the composer rather than raising bubbles.**
A selection drawn over a widget that keeps editing underneath it is a
selection that is wrong as soon as anything is typed. The upstream one is
integrated with the edits.

**Double-click on a row selects a word.** Consistent with the rest of the
frame, and it spends the gesture every list of openable things spends on
opening. Dragging still selects the row's text.

## Consequences

- Native terminal selection is gone from the whole window, not half of it, and
  so is the terminal's own middle-click paste. Shift-drag is the way back to
  both, and the help says so.
- `zones` is a new per-frame structure every screen's renderer contributes to.
  A control drawn without marking itself is a control the mouse cannot reach —
  the failure is silent, which is what `REVIEW.md` in the package now carries a
  rule about.
- `hints()` changes shape from a joined string to key/label pairs, across every
  screen that draws one.
- `charm.land/bubbles/v2` moves from v2.1.1 to v2.2.1 for the textarea's
  selection API.
