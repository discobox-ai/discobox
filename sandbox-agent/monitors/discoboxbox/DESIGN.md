# discobox-box Design

The image's `discobox-box` monitor
([ADR 26-11-01-560](../../../docs/adr/26-11-01-560-discobox-box-watches-other-boxes-through-the-sandbox-event-stream.md)),
a [built-in written in Go](../DESIGN.md#built-ins-written-in-go) watching
other discoboxes. One run follows the project's sandbox event stream
([ADR 26-10-29-316](../../../docs/adr/26-10-29-316-sandbox-state-changes-are-a-resumable-event-stream.md))
from the cursor in `discobox-box.json` for its interval (at most 90s), reports what each
event changes for the boxes it watches, and, when requests are watched, then
lists the secret requests this box may read.

```mermaid
flowchart LR
  Run["one run, for its interval"] -->|"GET /api/projects/P/events, Last-Event-ID"| Gate["pool gate (DISCOBOX_API_URL)"]
  Run -->|"then GET /projects/P/secret-requests"| Gate
  Gate --> Server["discobox API, as this box"]
  Run <-->|"cursor, every box's last state"| State[("discobox-box.json")]
  Run -->|"box.* events"| Runner["runner's queue"]
```

## Rules

- **The API is reached as the box's `discobox` CLI reaches it.** The base is
  `DISCOBOX_API_URL` (else `DISCOBOX_SERVER`), the project
  `DISCOBOX_PROJECT` (else `default`, the box's own). The gate adds the
  pool's credential; the box sends none. A refusal — no `discoboxApi`, a
  pool not yet connected — is the run's failure, with the server's text.
- **A run is a polling run that holds the stream.** It reads for
  `Env.Interval` (the runner's, so the instance's `interval`), at most
  `maxHold` (90s): the pool's gate ends any call to the API at two minutes,
  body included (`proxyagent.gateHTTPTimeout`). The runner starts the next
  run when the interval is up, at once for one held whole. A stream that ends
  sooner ends the run without failing it. Failure and recovery stay the
  runner's (ADR 26-11-01-477's reason against a built-in that streams
  forever).
- **The stream is project-wide; selection is here.** The route has no filter
  (ADR 26-11-01-560 §1). Every box's last state is held, watched or not, so
  a tag added later or options changed in place select from what is held. A
  box is watched when named by `id`, or when it matches every `tag` selector
  (`sandboxmeta`'s) and `created-by`, those given. `created-by=me` is this
  box's ID from its sandbox config; the event's `parentSandboxId` selects
  who leads it, while the persisted option keeps its `created-by` spelling.
- **Events are read through a local struct**, never `api/gen`'s types:
  runtime code does not use the public API's DTOs (the root `REVIEW.md`).
  Each field mirrors `SandboxEventData` in `server.yaml`. A box is called by
  its configured name, the event's; its state is the event's `displayState`,
  which the server combines by its one rule (`Sandbox.DisplayState`). It
  never combines the axes itself.
- **One event per change, keyed per box.** A box's state, its terminal and
  each request have their own key, so an unpopped event is replaced by the
  next of its kind. A terminal is reported only while its box runs.
- **First sight is a baseline, for what was already there.** The first run's
  replay reports nothing. A box first seen in a `sandbox.created` recorded
  after the baseline is `New`, and so is any box first seen in a reset's
  replay, since every box the project had is held. A `New` box is reported
  created when it is first watched, however late its tag comes. Any other
  box newly watched is recorded quietly, except a parent change: adoption is
  `box.adopted` (newly watched), and release or adoption away is one `box.gone`,
  even when only terminal changes are watched.
- **A replay is whole at the ID-only event that ends it.** The server ends
  every replay with where the stream stands, even where that equals its last
  event's ID (ADR 26-11-01-560 §3). How long the run held the stream proves
  nothing: a call the gate cuts, or one still reading, has not had it.
- **The cursor is saved only where it is safe to resume from.** After each
  event, except during a replay, a first run's or a reset's: then it stays
  where it was until the replay's ending event arrives, and after a reset the
  boxes the replay left out are taken as deleted. A run cut off before that
  starts from the current state again, or is reset again. A replay from
  scratch over boxes a cut-off one left held confirms them as a reset's does,
  so one deleted meanwhile is gone, but marks no box new: what is not held
  may be what the cut replay never reached.
  State is written with each event emitted, and at the run's end.
- **Requests are listed at the run's end**, once the boxes they come from
  are known, and also after a stream that failed for its own reason, so a
  stream that keeps failing hides no request; not after the API refused this
  box, which it would again.
- **A request is news when it was made after the baseline**, whichever box
  made it and however late that box came to be watched: it waits on
  someone. One made before the baseline is where things stood. The server
  lists only the requests of the boxes this box leads.
- **A watched box deleted is gone.** A named one is recorded gone whatever
  `watch` says, and an instance watching only ids ends
  (`discobox.finished`, final) once every one is gone. A named id is the
  run's failure when the project's state is known whole without it: after a
  replay ended, or in a run that went on from a cursor.
- **The box's clock** sets the baseline; an event's `at` and a request's
  `createdAt` are the server's. They are compared directly.
