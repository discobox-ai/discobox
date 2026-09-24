# 0134 — A tool put away is a tab after the shells

- **Status**: Accepted
- **Date**: 2026-09-18
- **Supersedes**: [26-08-27-302](26-08-27-302-a-tool-session-is-an-exec-the-launcher-labeled.md)
  §3 (a tool session is never a tab). §1–2 and §4–5 stand; §4's "puts it away"
  now means into the strip described here.

## Context

ADR 26-08-27-302 took a tool session out of the workspace's strip entirely: it is a
window over the workspace while it is showing, and nothing at all while it is
put away. The only way to know a diff or an editor is still running was to open
the tools picker and read `· running` on its row.

In use, that is the wrong half of "outlives being looked at". A tool put away is
a session you mean to come back to, and a session with no mark on screen is one
people forget is running, reopen from scratch, or never close. The window
already has a place for sessions you opened by hand and may come back to: the
shells column.

## Decision

1. A tool's pane lives in the shells column whether or not it is showing, and
   sorts **after** every shell (`execBefore`), so a shell opened later still
   goes ahead of it. It wears a digit like any tab there.
2. Showing a tool gives that pane the whole window, as before. Putting it away
   (`[-]`, the leader's `d`) leaves it as its tab, drawn at the column's width,
   with the focus back where the workspace had it. With no shells, the tool's
   tab is what opens the right column.
3. On a tool's tab in the split, the column's `[+]` and the leader's `z` mean
   the tool's own window, not the column's maximize. With the column already
   maximized they restore it, as on any tab, so a tool left as the last tab
   cannot hold the window with no way back to the split. The column's `[x]`
   and the leader's `X` end it, the same act as the tool window's `[x]`.
4. A tool picked up off the listing on attach arrives the same way: as a tab
   after the shells, not showing.

## Alternatives rejected

**A third column, or a strip of its own under the boxes.** Rejected: it costs a
row or a column's width on every workspace for something that is usually one
tab, and gives the tools a second set of navigation keys. The shells column is
already the "what you opened by hand" side.

**Keep the tool full size while it is a tab.** Rejected: a pane is drawn at the
size its far end was told, and a full-width screen cut down to half a window is
unreadable. The session is resized each way instead, which the tools redraw on
like any terminal program.

## Consequences

- A tool now splits the window when it is the only thing on the right; a
  workspace with a diff put away is no longer full width.
- A shell opened while a tool is put away renumbers the tool's tab, never the
  shells: tools are last.
- The strip's key map applies on a tool's tab. While the tool has the window
  the keys that would move or open something underneath it answer instead of
  acting.
