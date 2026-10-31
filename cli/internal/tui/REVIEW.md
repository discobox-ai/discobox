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
  The list in the workspace tree is the same trap from the other side: while
  it has the keys beside a page (`listHasKeys`), the page's keys, hints and
  paste are not the page's, and every one of the three asks it — a page
  whose key line kept offering its own keys offered Enter as "bind" while
  Enter attached the box under the list's cursor.

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

- **The window's terminal is asked for DA1 once, by the program-status
  probe, and every DA1 answer is taken as that probe settling** (`Update` →
  `isProbeSettled`). A second DA1 request from the window, or from a newer
  Bubble Tea, never sees its answer: the probe swallows it. Anything else that
  needs DA1 shares the probe's.

- **Every console `Model` frame is on the alternate screen.** Nothing is printed inline: rows
  left on the primary screen surface again on the way out or around a
  `tea.Exec`, and nothing here erases them. `View` stamps `AltScreen`; a
  builder does not.
  The standalone `ComposePrompt` is deliberately inline: it owns and clears
  its rows, starts at two editable lines, and grows without hiding shell history.
  Wrap its footer explicitly and budget those rows when sizing the editor,
  so narrow terminals show both the submit and discard-confirmation keys.

- **A window that is one command's own (`Model.oneShot`) never lands on the
  list.** Anything new that leaves the workspace decides where it starts what
  that window does instead — `Model.exit`, or a wait screen until it can
  (`archiveWorkspace`) — rather than inferring it later from screen state. An
  archive from the workspace once fell through to the launcher's list, and a
  first fix inferred the exit in `verbDone` from `attach != nil` and no panes,
  which also fired on unrelated verbs and showed the list while it waited.

## The workspace tree

- **A workspace's state is a field of `workspace`, never loose on `Model`.**
  Parking moves the whole value aside and unparking moves it back, so a field
  added beside it on `Model` is one that silently belongs to whichever box is
  on screen after a switch. What is the screen's alone — the generation, the
  opens in flight, the overlay — is ended in `endLiveScreen`, the one teardown
  closing and parking share.
- **Anything a pane started that answers it later finds it with `heldPane`.**
  A parked workspace's panes are still attached and still running; an issue
  read, a files changed session or an image paste that looks its pane up on
  screen only drops the answer for good once the box is switched away from —
  an issue tab froze that way. What `updateParkedPane` refuses a parked pane
  is refused on every `heldPane` path as well: a clipboard write (OSC 52)
  from a box that is not on screen is dropped, or a box switched away from
  replaces what the user just copied from the one they are in.
- **A walk that ends anywhere but the panes leaves no leader armed.** A run
  of the leader's Ctrl-↑↓ re-arms the leader in the pane it lands in; a walk
  that stopped on a row gave the list the keys, and a leader re-armed in the
  pane left behind took the next Ctrl-K or Ctrl-J typed there as another
  walk. Re-arm only while `inPanes()`.
- **A bare attach's chrome is the only thing a box cannot draw.** The box's
  terminal has every row but the status line, so a dialog the window puts up
  is told from one the box draws by covering that line too: every dialog,
  card and band stands in place of the whole window, and the window returns
  to its chrome for the credential band. Nothing may be drawn over the box's
  rows as though it were the window's own, and nothing may leave the status
  line standing under a prompt for a secret.

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

## The files changed

- **A files changed session ends on every way out.** The listing never shows
  one (`Exec.FilesChanged`), so whatever lets go of its window without
  ending it — closing a tab, replacing it, detaching, quitting the console —
  leaves it running where nothing will find it. Ends go through
  `reviewWindow.end`, counted in `reviewEnds`, and `Run` waits for them
  (`EndReviews`): an end started from a goroutine on the way out is lost when
  the process exits first — `TestQuittingTheConsoleEndsTheFilesChanged`.
- **A files changed window with no session to send to has its own keys.**
  While it starts, and once it ended on its own, every key would go to a
  terminal with no stream and be dropped, the way back included; `reviewKey`
  takes `q` and Esc then — `TestFilesChangedThatDoNotStartAreGivenUpOn`.


- Credential consent and Git prompting belong to the active operation's
  narration feed. Serialize them across concurrently delivered sources; a
  stale feed or dismissed question must release the worker with a decline.
  Return terminal ownership through `tea.ExecProcess`, and keep Git credential
  stdout captured by the delivery layer, never rendered.
- Never pair the listing's name (`Sandbox.Name`) with a program's `msg`: it
  can hold that msg (`sandboxmeta.Title`, ADR 26-10-31-618), so a record or
  line that shows both says it twice, once from a poll that lags the stream.
  Name a box without its program's words where the msg goes beside it
  (`recordTitle`).
