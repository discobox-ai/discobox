# tui — review rules

## The mouse and the hit map

- **A control you draw must mark itself** (`zones.mark`, the `markRow` /
  `markList` helpers, and `viewHints` for a key line). The window has no widget tree, so a control that is
  drawn but not marked is simply unreachable by the pointer — and the failure
  is silent: nothing logs, nothing looks wrong, the click just does nothing.
  Adding a row, a band, or an offer to a key line means adding its mark in the
  same place it is drawn.

- **Never compute a position twice.** Mark from the numbers the draw used, at
  the origin the composing code pushed (`zones.push`/`pop`), never from a
  second pass over the layout arithmetic. A hit map recomputed from the layout
  drifts from the frame the way a column header budgeted apart from its rows
  drifts out of line when a column drops — which is why `tailColumns` budgets
  both from one arithmetic — and there are no pixels to show it.

- **A hint that names a key carries the key.** `hints()` returns `hint` pairs,
  not formatted text. A new offer written as `says("x archive")` looks right on
  screen and is dead to the pointer; use `keyed`/`pressing`.

- **Shade from the same numbers you mark with.** `zones.hovering` takes the
  rectangle the mark took, at the same origin, in the same walk. A hint drawn
  as live somewhere other than where it is pressed is a button that lies about
  itself.

- **A link is a control, not just an escape sequence.** An OSC 8 link in the
  chrome answers Ctrl-click through the terminal; a plain click reaches the
  window instead, so the span it occupies is marked (`urlHit`) and lit from the
  same walk that draws it (`paneHeaderFields.render`). Adding a link to a
  header field means adding it as a `headerSpan` with its URL, not as more
  text in an existing span — an unmarked one is a link that silently does
  nothing when it is clicked the way links are clicked.

- **A control that answers a press outright must take the gesture**, so it does
  not also start a drag-select of its own label (`press` returns whether it did).
  One that only points at something — a row — must not, or the row's text stops
  being selectable.

## Selection

- **Everything on screen is inside the frame `paintChrome` is handed.** The
  chrome selection addresses the grid parsed from that frame, and the
  selection's `snap` (`github.com/discobox-ai/x/selection`) *clamps* a press
  past the last line instead of dropping it.
  So a row appended to the view after the paint is not inert: it silently hands
  its presses to the row above it, and the only symptom is a drag that
  highlights the wrong line. Anything that wants to sit outside the window —
  under the border, beside it — goes on an existing row instead.

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
  pane (`inPanes`), then the harnesses/secrets screens, then the console.
  `updateKey`, `updatePaste` and `hints()` must follow the same order.
  The harnesses and secrets screens are the trap: they open panes of their own
  and stay open behind them, so a check on `harnessesOpen`/`secretsOpen` that
  is not guarded by `!m.inPanes()` steals every key from the terminal drawn
  over it — and on that screen the stolen keys are commands: Enter in a
  harness's configure terminal becomes "reconfigure the highlighted harness"
  and restarts the flow it was typed into.

- **Input held behind slow work never holds the leader.** An image paste holds
  a pane's keys while it reads the clipboard and copies into the discobox,
  which can take minutes over a slow link; the leader and the key after it are
  the window's own commands — quit, detach, move — and holding them too left
  no way out until the copy ended.

## The screen

- **A frame the window holds only briefly is a frame that may never be drawn.**
  The renderer keeps the latest frame and writes whichever is current when its
  own clock fires, so anything a frame is meant to *do* cannot be timed out
  with a pause of ours.

- **What the window sets on its terminal that Bubble Tea does not reset, it
  takes back twice**: on its own way out, once the program has ended
  (`Run` → `clearProgramStatus`), and in `termguard`'s reset for a window that
  died. `termguard` writes only after a crash, so a reset kept there alone is
  missing from every ordinary quit — the OSC 7501 record the window speaks
  for its box would stay on the user's tab, claiming the box is working long
  after the window that knew is gone.

- **Every frame is on the alternate screen.** Nothing is printed inline: rows
  left on the primary screen surface again on the way out or around a
  `tea.Exec`, and nothing here erases them. `View` stamps `AltScreen`; a
  builder does not.

## Saved state

- **The window's memory is one per machine, shared by every window open at
  once** (`Session.Draft`, `View`, `LastSource`; ADR 26-10-20-596). "Nothing
  changed since this window last wrote it" is not "the store still holds it":
  another window may have written since. Anything that skips a write on that
  basis must still write what must survive on the way out (`saveDraftNow`).

## Tests

- **A command that waits by design goes through `listen`; a timer through
  `tick`.** Never `tea.Tick`, and never a bare read of a channel, a select or
  a follow in a returned command: a test runs every other command to its end
  and fails, naming it, on one that does not finish (`TestABlockingCommandIsNamed`).
  Nothing that does finite work is wrapped, or a test never sees it work.
- **A driver test waits for what it asserts.** A fake records a call from the
  command's goroutine, before the window has the answer: wait on the window's
  state, not on the fake's.
