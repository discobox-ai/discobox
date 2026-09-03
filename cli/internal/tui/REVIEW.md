# tui — review rules

## The mouse and the hit map

- **A control you draw must mark itself** (`zones.mark`, and the `markList` /
  `markHints` helpers). The window has no widget tree, so a control that is
  drawn but not marked is simply unreachable by the pointer — and the failure
  is silent: nothing logs, nothing looks wrong, the click just does nothing.
  Adding a row, a band, or an offer to a key line means adding its mark in the
  same place it is drawn.

- **Never compute a position twice.** Mark from the numbers the draw used, at
  the origin the composing code pushed (`zones.push`/`pop`), never from a
  second pass over the layout arithmetic. A hit map recomputed from the layout
  drifts from the frame exactly the way the column header drifted from its rows
  before `tailColumns` — with no pixels to show it.

- **A hint that names a key carries the key.** `hints()` returns `hint` pairs,
  not formatted text. A new offer written as `says("x archive")` looks right on
  screen and is dead to the pointer; use `keyed`/`pressing`.

- **A control that answers a press outright must take the gesture**, so it does
  not also start a drag-select of its own label (`press` returns whether it did).
  One that only points at something — a row — must not, or the row's text stops
  being selectable.

## Selection

- **One selection on screen at a time.** A press that starts one clears the
  others (`clearSelections`, `clearPaneSelections`); two highlights racing to
  answer the next copy is the bug this prevents.

- **A selection whose text no longer reads back identically is cleared**, never
  left highlighting whatever the recompose moved under it (`paintChrome`).

## Layout

- **The composer's height is a function of its contents**, so `layout()` runs
  after every key *and* every mouse event — a press that changes what is in the
  field changes where everything under it is drawn.
