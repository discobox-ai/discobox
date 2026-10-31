# 26-10-31-319 — A press on a workspace tree row switches to it

- **Status**: Accepted (supersedes [0088](0088-the-launcher-window-answers-the-mouse-and-owns-selection.md) §4 and §2 for the workspace tree)
- **Date**: 2026-10-31

## Context

The console's workspace can be drawn with the discobox list beside it: a
narrow column down the left (the workspace tree, `cli/internal/tui/tree.go`)
listing the project's discoboxes, with the box on screen picked out. Switching
boxes there parks the workspace on screen — its streams stay attached, its
panes keep emulating — and shows the chosen one's, so switching is instant and
switching back costs nothing.

[ADR 0088](0088-the-launcher-window-answers-the-mouse-and-owns-selection.md)
settled how the window answers the mouse on its lists:

- §4: a left press on a row moves the cursor to it, focuses its list and goes
  on into the chrome's selection, so the row's text stays drag-selectable; a
  double-click acts on it.
- §2: selection outside a pane is the chrome's, on every screen.

Read on the tree, that is a press that picks a box without showing it — the
list's cursor on one box, the workspace beside it showing another — and a
second press to do the one thing the tree is for.

## Decision

### 1. A press on a tree row switches the workspace to that box

A left press on a row of the workspace tree shows that box's workspace: its
parked one when it has one, a fresh attach when it has none. The list's
cursor goes to the row, and the keys go to the workspace shown. A press on the
row of the box already on screen gives the list the keys instead, as
`leader h` does, so the keyboard can carry on from there.

The right button opens that row's actions menu, as §4 has it on the list
screen, and the wheel moves the list's cursor without moving focus, also as
§4 has it.

### 2. The tree's text is not selectable

A press over the tree is the tree's, not the start of a chrome selection: the
chrome selection covers the window right of the tree, which is drawn and
measured as though the tree were not there. A drag that began in the window
and ends over the tree is still the window's.

Every name the tree shows is on the expanded list (`leader z` on the tree),
which is the list screen itself, where §2 and §4 hold unchanged, and in the
workspace header of the box on screen.

## Alternatives rejected

**§4 as it stands: the first press picks, the second acts.** The tree is a
switcher, and the workspace beside it is the one thing it switches. A picked
row whose box is not on screen leaves the tree and the screen disagreeing
about which box you are in, until a second press or Enter settles it — and the
press that would settle it is the one every user makes first. On the list
screen the second press opens something that was not open; here it shows
something already attached and waiting, and showing it is undone by a press on
the row you came from.

**A chrome selection over the tree too.** The window right of the tree is
composed, painted and selected in its own coordinates, so that nothing in the
workspace knows the tree is there. A selection across both would have to be
measured over the joined frame, and every pane's mouse translation and the
chrome's hit map with it — a rework of the selection's coordinate system for
text that is already selectable where it is the whole screen.

## Consequences

- On the tree, a mistaken press switches boxes. Nothing is lost: the box left
  is parked, still attached, and a press on its row brings it back as it was.
- Dragging across a box's name in the tree selects nothing. The expanded list
  and the workspace header are where its text is copied from.
- [ADR 0088](0088-the-launcher-window-answers-the-mouse-and-owns-selection.md)
  §4 and §2 hold everywhere else: the list screen, the expanded tree, the
  harnesses, secrets and skills, the dialogs.
