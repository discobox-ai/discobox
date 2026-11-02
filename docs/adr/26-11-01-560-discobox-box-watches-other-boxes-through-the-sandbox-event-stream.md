# 26-11-01-560 — `discobox-box` watches other boxes through the sandbox event stream

- **Status**: Accepted
- **Date**: 2026-11-01
- **Amends**:
  - [26-10-31-073](26-10-31-073-a-box-runs-the-monitors-its-agent-enables-and-queues-their-events-until-it-pops-them.md)
    §9, the image's built-in monitors, by one.
  - [26-10-29-316](26-10-29-316-sandbox-state-changes-are-a-resumable-event-stream.md)
    §1's fields, by `tags` and `createdBySandboxId`, and its refusal of the
    stream to a sandbox principal (Consequences).
- **Relates to**: [26-11-01-477](26-11-01-477-github-repo-is-a-go-built-in-that-probes-before-it-reads-and-an-instance-may-choose-its-interval.md)
  §1, the built-in seam it runs on, and its rejection of a streaming
  built-in;
  [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §9, what a box with `discoboxApi` may call;
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md),
  a terminal's status.

## Context

A lead discobox running `orchestrate-issues` watches its workers with
`orchestrate.sh`: it polls `discobox admin box ls` for their state, takes a
worker as idle from its terminal's title and screen text, and lists pending
secret requests to approve them (#427). The loop dies with the harness
session. Monitors replace it, and the lead's box-side half needs one that
watches boxes.

What a box can read (880 §9), when a person has set `discoboxApi` on it:

- **`list-sandboxes`**: every box in its project, with each one's state,
  tags, the box that created it, and the status its primary terminal's
  program last reported (424).
- **`get-sandbox`**, a terminal's screen and its events: only for the boxes
  it created.
- **`list-secret-requests`**: only the requests of the boxes it created.
- **The sandbox event stream** (316): refused, "until a consumer needs it
  scoped to the discoboxes it created". Its events carry each box's
  existence, power and program state, but not its tags.

## Decision

`discobox-box` is a built-in written in Go on 477 §1's seam: the image's
`50-discobox-box.sh` execs `discobox-sandbox-agent monitor-builtin
discobox-box`.

### 1. A box may read its project's event stream

The sandbox role gains `GET /projects/{projectId}/events`, for the whole
project, as `list-sandboxes` already is. Every field an event carries is in
that listing, so the stream shows a box nothing it cannot already read. A
consumer narrows it to the boxes it cares about on its own side. The route
has no filter, and a box gets none.

### 2. An event carries the box's tags and its creator

`SandboxEventData` gains:

- `tags`: the box's tags, as `list-sandboxes` serves them. A tag change is
  an event. Tags change when a person or a box sets them, not on every turn,
  unlike the title and msg 316 §1 leaves out.
- `createdBySandboxId`: the box that created it, when one did. It never
  changes, so it adds no events.

The store's meta write records an event when the tags it writes differ. On
start, the backfill that wrote each sandbox's first event also appends an
update for every sandbox whose latest event differs from what the store now
projects, so a stream opened with no cursor shows every box's tags from the
upgrade on.

### 3. Each run reads the stream from its cursor, for its interval

A run connects with the last event ID it saved, reports what each event
changes, saves the ID after each one, and ends once its interval has passed.
The runner starts the next run straight away, so changes are reported as
they happen, and the instance keeps a polling monitor's failure and recovery
(477 rejected a built-in that streams forever because a runner sees neither
until it exits). The interval is 30s by default and no less than 10s (477
§2's `interval` option).

- **The stream is followed, not compared.** Each event is a box's whole
  state; the monitor keeps the last state of every box in the project, and
  reports what differs for the boxes it watches.
- **A `reset`** marks every box held unconfirmed. The replay that follows
  confirms the ones that still exist, and at the run's end any left
  unconfirmed are taken as deleted. A box the reset found changed is
  reported as changed.
- **Secret requests are not on the stream.** When requests are watched, each
  run lists them once, at its start.

### 4. What it watches and reports

- **A box is named by `id`, selected by `tag`, or by `created-by`** (a box
  ID, or `me` for this box). The options are matched in the monitor:
  `pkg/sandboxmeta`'s selectors for tags, the event's `createdBySandboxId`
  for its creator.
- **Events**, each keyed per box so an unpopped event is replaced by the
  next of its kind:
  - its state changing (`box.state`), a box created since the baseline
    (`box.created`), and a watched box deleted (`box.gone`);
  - its primary terminal's program reporting a new state (`box.busy` for
    `working`, `box.idle` for the rest: idle, done, blocked with its kind,
    error), from the reported status, never from screen text;
  - a secret request from it pending (`box.request`) and answered
    (`box.request.answered`), for the boxes this box created.
- **A box is called by its configured name**, the name the event carries.
  The display name follows the program's title, which the stream leaves out
  (316 §1).

### 5. Baselines are 073 §3's, for what was already there

The first run's replay of the project is recorded and reports nothing. A box
that existed at the baseline and is selected later, by its tag or by options
changed in place, is recorded quietly. A box created after the baseline is
news whenever it is first selected, since a worker tags itself once it runs.
A request made after the baseline is news from any watched box; one made
before it is where things stood. An instance watching only ids ends once
every one is gone.

## Alternatives rejected

- **Poll `list-sandboxes` every run and report the difference.** It is what
  this ADR first decided. It reports a change up to an interval late, and
  reads every box in the project every interval to find that one changed.
  The stream sends each change once, and an idle project costs a connection
  that says nothing.
- **Scope the stream to the boxes the caller created**, as 316 left open.
  A lead watching a peer it was told to watch could not, and the listing
  already shows a box every box in its project. The creator is in the event
  instead, so a consumer that wants only its own boxes says so itself.
- **Learn tags from `list-sandboxes` once per run, leaving the event
  alone.** A worker tagging itself `ready` would be seen only at the next
  run, and every consumer of the stream that selects by tag would need the
  same second read.
- **A streaming built-in holding the stream for good.** 477's reason stands:
  the runner reports a failure only when the run exits, and a recovery only
  when one succeeds.
- **Read each box with `get-sandbox`.** A request per box per run, and
  refused for any box the caller did not create.
- **Infer idle and busy from the screen**, as `orchestrate.sh` does. 424 §3
  makes the program's report the status; a screen's wording is not a
  contract.

## Consequences

- A box running `discobox-box` calls the server. `sandbox-agent`'s rule
  against calling back to the server is the agent's own: a monitor is a
  process of the box, run as its user, reaching the API through the gate as
  `discobox` does, not the agent fetching its configuration.
- `discobox events` works in a box with `discoboxApi`, and shows tags.
- The server-sent events parser moves from the CLI to `pkg/sse`, which both
  use.
- A request from a box this box did not create is never reported, and the
  skill says so.
- `orchestrate-issues` can watch its workers with `discobox-box -o
  created-by=me`, and stop scraping screens and polling `box ls`.

## Amendments

Amended before anything shipped against it: it first decided that each run
polls `list-sandboxes` and reports the difference, and rejected the event
stream because a box was refused it and its events carried no tags. It now
opens the stream to a box (§1), adds tags and the creator to the event (§2),
and reads the stream (§3). `created-by` is new, and a watched box selected
by tag is reported gone when deleted, since the stream says so.
