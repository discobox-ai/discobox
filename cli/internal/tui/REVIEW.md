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

- **Shade from the same numbers you mark with.** `zones.hovering` takes the
  rectangle the mark took, at the same origin, in the same walk. A hint drawn
  as live somewhere other than where it is pressed is a button that lies about
  itself.

- **A control that answers a press outright must take the gesture**, so it does
  not also start a drag-select of its own label (`press` returns whether it did).
  One that only points at something — a row — must not, or the row's text stops
  being selectable.

## Selection

- **Everything on screen is inside the frame `paintChrome` is handed.** The
  chrome selection addresses the grid parsed from that frame, and
  `selection.snap` *clamps* a press past the last line instead of dropping it.
  So a row appended to the view after the paint is not inert: it silently hands
  its presses to the row above it, and the only symptom is a drag that
  highlights the wrong line. Anything that wants to sit outside the window —
  under the border, beside it — goes on an existing row instead; see
  `initializing.go`, which is there because it tried the other way.

- **One selection on screen at a time.** A press that starts one clears the
  others (`clearSelections`, `clearPaneSelections`); two highlights racing to
  answer the next copy is the bug this prevents.

- **A selection whose text no longer reads back identically is cleared**, never
  left highlighting whatever the recompose moved under it (`paintChrome`).

## Layout

- **The composer's height is a function of its contents**, so `layout()` runs
  after every key *and* every mouse event — a press that changes what is in the
  field changes where everything under it is drawn.

## Key routing

- **What draws in front takes the keys.** `View`'s precedence is the contract:
  a modal (`modalUp` — the introduction, a dialog, the run options), then a
  pane (`inPanes`), then the harnesses/secrets screens, then the launcher.
  `updateKey`, `updatePaste` and `hints()` must follow the same order.
  The harnesses and secrets screens are the trap: they open panes of their own
  and stay open behind them, so a check on `harnessesOpen`/`secretsOpen` that
  is not guarded by `!m.inPanes()` steals every key from the terminal drawn
  over it — and on that screen the stolen keys are commands, so Enter at a
  harness's configure banner meant "reconfigure this harness" and restarted the
  flow it was typed into.

## The screen

- **A frame the window holds only briefly is a frame that may never be drawn.**
  The renderer keeps the latest frame and writes whichever is current when its
  own clock fires, so anything a frame is meant to *do* — the empty inline frame
  that erases the opening prompt (`clearPrinted`) — cannot be timed out with a
  pause of ours. Wait for the terminal to answer (`clearAcks`) instead.

- **The alternate screen does not take the printed rows with it.** Anything
  drawn inline stays on the primary screen, behind the window, and surfaces
  again on the way out or around a `tea.Exec`. A new door onto the whole
  terminal belongs in `takesScreen`, which is what `clearPrinted` reads.
