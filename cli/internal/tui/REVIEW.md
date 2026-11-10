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

- **Every key the list, the tree, the window's leader and the panes bind is
  the registry's** (`cli/internal/console`, `commands.go`): a command's, or a
  gesture's the registry declares. A name the window
  spells a key by is read from it (`bareKey`, `leaderKey`, `chordKey`),
  never a string written here, and the leader's keys outside a pane are
  answered by command ID (`leaderCommand`), not by key. A new key goes in the
  registry first; `TestConsoleCommandBindingCoverage` fails one bound only in
  a switch or a pane option, and `TestTheHelpNamesEveryBoundCommand` one the
  help does not name. A key's printed name goes through `keyLabel`, never a
  spelling of its own (`strings.ToUpper` on `f3`): one moved to a chord would
  print wrong.
- **How a terminal spells a key is folded in the key path, never bound
  twice** (`foldChord`, `unshiftBackspace`): the registry holds one key per
  binding, and the window folds what a terminal reports onto it before
  anything reads the key. A chord on a punctuation key is the trap: under the
  Kitty protocol's first level a terminal reports the key pressed on the
  layout in use, so Ctrl-: is Ctrl-Shift-; on a US layout and Ctrl-Shift-. on
  a German one. A new such chord gets each layout's spelling folded and
  tested, or the key line offers a key that does nothing there.
- **One command, one implementation, whichever way it is reached.** The
  window's leader handling outside a pane dispatches a command it shares
  with the panes — the walks between panes and boxes — by ID rather than
  doing its work inline, so the palette, a pane's leader key and the
  window's leader enter the same `Frontend` method. Next pane from
  the palette once moved focus in a hidden workspace while the leader's `l`
  handed the list's keys back, because the two were separate code.
- **A `console.Context` field is filled from a `Model` method, never a
  parallel expression** (`commandContext`): the list holding the keys is
  `listInFront`, which the pane walk also acts on, beside `listHasKeys`,
  which the routing reads. A field filled from an expression of its own
  drifts from what the window does, and the palette offers what a key does
  not.
- **A renderer never rewrites an `Entry`'s `Enabled` or `Why`.** Whether a
  command can act is the registry's, and the palette, menus and keys must
  agree on it; a fact the rule needs from the window (the list has the keys,
  the box whose workspace is open) goes into `console.Context` instead. Both
  times this was got wrong, a menu offered what the palette greyed.
- **A command travels by its registry ID** — menu rows (`action.key`), pane
  messages, `runActionMsg` — never by the key that pressed it. The key is how
  it was reached; the ID is what it is.
- **What draws in front takes the keys.** `View`'s precedence is the contract:
  a modal (`modalUp` — a dialog, the run options), then a
  pane (`inPanes`), then the harnesses/secrets screens, then the console.
  `updateKey`, `updatePaste` and `hints()` must follow the same order.
  The harnesses and secrets screens are the trap: they stay open behind a
  workspace drawn over them, so a check on `harnessesOpen`/`secretsOpen` that
  is not guarded by `!m.inPanes()` steals every key from the terminal drawn
  over it — and on those screens the stolen keys are commands.
  The list in the workspace tree is the same trap from the other side: while
  it has the keys beside a page (`listHasKeys`), the page's keys, hints and
  paste are not the page's, and every one of the three asks it — a page
  whose key line kept offering its own keys offered Enter as "bind" while
  Enter attached the box under the list's cursor.
  The audit screen is the trap a third way: it is drawn over the panes, so
  anything that reaches a page from it — the leader's F3, say, to the
  harnesses page — closes it first (`openPage`), or the audit comes back in
  front, takes its keys into the audit search, and leaves nothing to click.

- **A dialog opened by a slow read waits behind a card of its own, and
  replaces only that card.** The answer arrives whenever the server gives it;
  opened unconditionally, it lands over whatever the person has moved to —
  a workspace, another dialog — and takes its keys, and clearing `m.busy` on
  the way ends somebody else's line. Put a `statusDialog` up, carry it in the
  answer, and open the result only while `m.dialog` is still it
  (`askSettings`). `showHarnessChanges` predates this and does not.
  A status card is not the window's wait unless it is marked `windowWait`:
  the create's narration writes into, and its end takes down, only that one.
  Testing `kind == dlgStatus` instead let a create's progress overwrite the
  settings card and close it under its read.

- **Input held behind slow work never holds the leader.** An image paste holds
  a pane's keys while it reads the clipboard and copies into the discobox,
  which can take minutes over a slow link; the leader and the key after it are
  the window's own commands — quit, detach, move — and holding them too left
  no way out until the copy ended.

## The screen

- **Bubble Tea's own messages arrive in no order.** The size it opened with,
  the color profile, the environment and every answer to `RequestWindowSize`
  are each sent from a goroutine of their own, so one asked for later is not
  newer. Never treat the last one to arrive as current because of when it was
  asked for. The window's size is re-read on every change instead
  (`WindowSizeMsg` in `update`): the opening size arriving after the
  window's own re-read left it drawing for 40 rows on a 38-row terminal
  (#182).

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
  A run's failures before its workspace — the create itself, the harness
  questions and setup — once only reported on the status line, which in such
  a window is the list; they go through `Model.runFailed`. Leaving the setup a
  run stopped for, and answering a question put up over the wait, were missed
  the same way. This is the third time: check every path a new modal or
  pane closes by — key, click, detach, failure — for what it uncovers.
  A box link pressed in a bare attach was the fourth: `landOn` would have
  parked the attached box and opened another, while `m.attach` and `made()`
  went on naming the first. A path reachable inside such a window that
  would switch to a *different* box decides what the window does instead
  (`focusAddressed` says how to open it); opening the window's own box, as
  a run does with the box it made, is what the window is for.

- **Text the window builds from a pane's output is box-written.** A link
  in a pane, and everything parsed out of one, was printed by the program
  in the box, and the status line is drawn outside every pane's emulator.
  This package has no escaping helper of its own (`terminalSafe` is
  `internal/cli`'s, cli/REVIEW.md), so such a value goes into a message with
  `%q`, never `%s` or `+` (`say` in `boxlinks.go`). Parsing makes it worse,
  not better: `ParseSandboxAddress` percent-decodes the path, so a link that
  is plain printable text turns into an ID carrying a real ESC.

## The workspace tree

- **A workspace's state is a field of `workspace`, never loose on `Model`.**
  Parking moves the whole value aside and unparking moves it back, so a field
  added beside it on `Model` is one that silently belongs to whichever box is
  on screen after a switch. What is the screen's alone — the generation, the
  opens in flight, the audit — is ended in `endLiveScreen`, the one teardown
  closing and parking share.
- **The busy line is the screen's to end only when the screen owns it.** A
  create's narration is the window's: it keeps reporting into the workspace
  the create goes to (`watchCreate`), so `endLiveScreen` leaves it running
  (`createNarrating`). Never save and restore `busyGen` around a park to keep
  a narration alive — the generation only goes up, and a restored one
  revives whatever owned the line at the save.
- **Two messages from one `tea.Batch` arrive in either order.** Each runs on a
  goroutine of its own, so a create's "server took it" (`promptSpentMsg`) can
  land after its "done" (`createdMsg`). What one learns of the other goes in a
  record both carry (`createWatch`), not in a window-wide field, which a
  second create started in between overwrites too. The acceptance now comes
  off the create's narration feed and so often lands after "done": whichever
  of the two gives the composer back marks it (`createWatch.spent`), and the
  other leaves the composer alone — it may be the next create's by then
  (`TestALateAcceptLeavesTheNextCreatesComposer`).
- **Every way a card closes reaches its `onCancel`** when the card stands for
  a question a goroutine waits on (`m.ask` marks it `waitedOn`): Esc and `q`
  alike. Only there: on other cards `q` just closes. A create's questions are
  such cards (`console.Create` asks them), so `q` on the uncommitted changes
  card is its Esc too, which the core answers as starting from the last
  commit. A card that closed without it once left a delivery waiting in `Ask`
  for the life of the window, holding the credential lock every other source
  waited on.
- Install an operation's progress status before launching its asynchronous
  work. Batching the start notice with the operation lets a fast completion
  arrive first; a late "merging…" once erased GitHub's merge refusal, and a
  late "copying…" an image paste's "disk full" (a Windows CI flake). Put it
  up with `m.update(status(…)())` and batch what that returns (its expiry): a
  handler already runs inside `Update`, which does its after-message work
  once, around the whole message.
- Never batch a one-time notice (an error, which does not time out) beside an
  operation that reports its own outcome. Whichever lands last stays, so the
  notice can replace the failure for good; and installing it first does not
  help, since the operation's progress replaces it at once, unread. Say it only
  on a path with nothing else to report, or in the operation's own report
  (`imagesRead`'s clipboard watch notice).
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

## The list

- **A change to the list's rows reads the cursor's box before it changes
  anything** (`sandboxList.keepingCursor`). The cursor is a row number, kept
  on its box by finding the box again after the change; a change made in
  place first — rows of `l.all` given a new state, as an archive's marks do —
  makes the box that was under the cursor one that is already gone, and the
  cursor lands on whichever box slid into its row. The first cut of the
  archive marks did that, and broke the tree's move to the next box after an
  archive.
- **Change the rows through `sandboxList`'s own methods, never the embedded
  `console.List`'s** (`setAll`, `markArchiving`, `unmarkArchiving`, not
  `SetAll`, `MarkArchiving`, `UnmarkArchiving`). The list embeds the core's,
  so the core's compile on it, and they skip `keepingCursor`: the cursor
  stays at its row number and lands on whichever box slid into it.

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
- **An attach test that needs other boxes waits for `m.list.Loaded()`.**
  The primary terminal can open before the first listing, since attach starts
  from its given box. Waiting for terminal output alone left the box-link test
  clicking before it could identify the other box and reporting it missing.
- **A driver test runs in `synctest.Test`.** Build the model, its fakes and
  their channels inside the bubble: one made outside it is not the bubble's,
  and a goroutine blocked on it holds the clock still. Inside, `time` is the
  test's clock, which moves only when the window is idle, so a wait, a settle
  or a sleep is exact on any machine. A goroutine a test leaves running fails
  it when the bubble ends; close what was opened rather than leaving it.

## The files changed

- **A files changed session ends on every way out.** The listing never shows
  one (`Exec.FilesChanged`), so whatever lets go of its window without
  ending it — closing a tab, replacing it, detaching, quitting the console —
  leaves it running where nothing will find it. Ends go through
  `reviewWindow.end`, counted in `reviewEnds`, and `Run` waits for them
  (`EndReviews`): an end started from a goroutine on the way out is lost when
  the process exits first — `TestQuittingTheConsoleEndsTheFilesChanged`.
- **A files changed window is let go only through `closeReview`.** It ends
  the session and closes the window's pane; a bare `v.review = nil` leaves
  the pane's input goroutines running for as long as the console does.
- **A files changed window with no session to send to has its own keys.**
  While it starts, and once it ended on its own, every key would go to a
  terminal with no stream and be dropped, the way back included; `reviewKey`
  takes `q` and Esc then — `TestFilesChangedThatDoNotStartAreGivenUpOn`.


- Credential consent and Git prompting belong to the active operation's
  narration feed (`console.Narration`). Serialize them across concurrently
  delivered sources; a stale feed or dismissed question must release the
  worker with a decline (`Asking.Decline`, `Handover.Done`).
  Return terminal ownership through `tea.ExecProcess`, and keep Git credential
  stdout captured by the delivery layer, never rendered.
- **A create's own questions are never declined as stale; what it asks once
  sending always is** (`narration.create`, `createWatch.sending`). Declining
  one of `console.Create`'s questions answers it — the uncommitted changes
  left out, or the create turned down — and any narrated operation started
  meanwhile (a verb, an attach) makes the feed stale. But the same feed then
  carries delivery's Git credential consent, and asked over whatever the
  person moved on to, an Enter meant for a box's terminal lends their
  credential. `sending` is set as the event is read, never through a message
  of its own, so it is ordered before what the feed says next. Only
  `console.ErrTurnedDown` ends a create quietly; any other cancellation is
  reported.
- **A one-command run's wait says nothing is created until the create is
  sent** (`createWatch.sending`, from `Narration.Sending`). The wait is up
  while the create still reads the tree and asks; Esc there, or a second Esc
  just after leaving a question, once exited 0 with no request made, under a
  line saying the discobox carries on.
- Never pair the listing's name (`Sandbox.Name`) with a program's `msg`: it
  can hold that msg (`sandboxmeta.Title`, ADR 26-10-31-618), so a record or
  line that shows both says it twice, once from a poll that lags the stream.
  Name a box without its program's words where the msg goes beside it
  (`recordTitle`).
- **What the approval card says a binding becomes follows the server's rule,
  fate by fate.** The routes a replaced binding gains are shown on whichever
  binding `bindingplan` keeps (`plan.Keep`: an `Updated` one, or the
  delegation of a `Split`), never on one fate alone, and "the same routes" is
  said only when the two lists are equal. A summary narrower than the rule
  tells the approver a widening is nothing — a first draft of `routeDiff`
  hid a split delegation's new routes that way.

## The leader

- **The window's own leader matching is a second copy of termpane's prefix
  rules, and must stay the same rules** (`updateKey`'s `leaderArmed` branch
  against `termpane.afterPrefix` and `handleKey`). Forgive the leader's own
  modifier on the second key and no other — no hand-written `ctrl+` forms,
  which under an Alt leader make Ctrl-L mean one thing on the list and another
  in a pane — and match the leader itself in full before forgiving anything,
  or the leader twice runs its bare letter's command (Ctrl-Q Ctrl-Q quitting).
  Both slipped in once, when the leader grew an Alt form.
- **A key the window takes ahead of termpane must yield to the leader**
  (`imageInput`'s image keys, and anything like them). `NormalizeLeader`
  accepts any Ctrl or Alt letter, so a key taken first is one some user's
  leader is, and a leader that cannot be pressed in a harness terminal leaves
  no way out of it.

## Harnesses

- **Nothing offers to set a harness up.** Every harness runs (ADR
  26-11-02-152); a harness is signed in inside a discobox and its change saved
  from the harness-changes band. A new action, card or band that sends somebody
  to configure a harness out here is the model this replaced; saying what
  signing in and saving expose is not.
- **A band's leader key is bound in two places**: the pane's prefix bindings
  (`pane.go`) and `updateKey`'s over-the-audit switch, since the audit screen
  covers the panes but still draws the band. The harness-changes band's H was
  added to the first and not the second, and over the audit it typed into the
  search.
- **Nothing unprompted that answers to single letters comes up over typing**:
  not over a focused terminal, nor over the composer while it holds a draft or
  was typed into in the last few seconds (`writingPrompt`). The explainer was
  first guarded against the terminal only, and the composer is the list's
  default focus.
- **A flag the box reports never takes down a refusal the control plane
  recorded.** `HarnessConfigPending` is the box's word; a refused login
  standing aside for it once let a box turn the red refusal into the green
  "review and save" offer.
- **The harness-changes band's occurrence is its candidate.** Keyed on the box
  alone, a dismissal would hide every later change; keyed on nothing, it would
  come back on every listing.
- **The explainer is recorded whichever way it is answered**, and only shown
  with nothing else in front (`explainHarnessChangesIfOwed`). A path that opens
  it over another dialog or a focused terminal — where the person
  is likely still typing their sign-in, and a typed letter answers it — or
  that records it without showing it, loses the one explanation the project
  gets.

- Scope addition-preview promises to the addition action. The add-or-replace
  dialog offers both outcomes; an unqualified footer saying the rest stays
  unchanged misstates replacement, which clears the existing use grants.

- A form submitting asynchronous work installs its wait dialog synchronously
  before returning the command. An intermediate submit message leaves a gap
  for queued input to open another dialog, which that submit would overwrite.

- Approval scope choices cover existing and newly created credentials, and each
  approved part independently. Wider-scope previews expose every independent
  credential constraint list, without flattening them; showing only the first
  list hides a live ceiling. Keep the selected scope through create-and-approve
  as well as ordinary approval.

- Refresh a mutable client preference used by a command preview separately
  from the initial Session. Replaying sessionLoaded restores drafts and filters
  over current edits; freezing saved Git consent left a preview authorizing
  lend after the delivery had correctly reread a revoked preference.

- Observe an exec through its live fake attachment, not simply its newest one.
  Shell creation and exec listing can attach concurrently; the workspace closes
  the losing duplicate, which may be the last attachment the fake created.


## Palette overlays

- Route a whole-window modal's mouse before the workspace tree translates or
  consumes it. Marking the overlay in screen coordinates while dispatching in
  right-pane coordinates silently broke its controls beside the tree.
- A dimmed bare status line is not modal provenance: with no color it is the
  original line. Replace the trusted final row with a window-owned modal label
  while an overlay is up, and test that distinction in NO_COLOR.
- Budget the input's prompt and end cursor within the card content width.
  A wrapped query moves every result below the rows the mouse was marked for.
- Keep a palette's temporary reveal across the empty-workspace teardown that
  precedes opening its live result. Restore it on leaving that result, not on
  the intermediate close used to start its workspace.

- A box command keeps the server from its selected snapshot through every
  follow-up and stream. Use the required server-scoped data source; never
  recover a server from the current box-ID listing map. Two independent
  servers can share an ID, and a newer listing can arrive before the UI sees
  it. Workspace, selection and tool-cache identities include the server too.
