# 26-10-29-316 — Sandbox state changes are a resumable event stream

- **Status**: Accepted (narrows [0081](0081-project-events-are-not-persisted-and-the-wait-polls.md)'s Decision and Consequences for sandbox state: sandbox events are persisted again, and the store keeps a per-transaction batch of them; amends [0034](0034-sandbox-state-and-runtime-state-are-separate-fields.md) §2: an empty `runtime_state` is also an archived sandbox its pool has synced without)
- **Date**: 2026-10-29
- **Relates to**: [ADR 0061](0061-the-client-facing-project-event-stream-is-removed.md)
  and [ADR 0081](0081-project-events-are-not-persisted-and-the-wait-polls.md),
  which removed the last stream and its table and said to build the next one
  "for the consumer that needs it, to that consumer's contract". This is that
  consumer. Nothing here brings back `project_events`, the broker, or the
  attach wait's subscription; the wait keeps polling.

## Context

Something outside the server — a dashboard, a notifier, a controller — wants
to watch a project's discoboxes: when one is created, archived or deleted,
when it powers up or down, and when the program in it starts working, finishes
or is blocked on a person. Today the only way is to poll `list-sandboxes` and
diff it.

ADR 0061 removed a stream that promised more than it delivered: it stamped a
`seq` that could not be resumed from, and its delivery was lossy. The consumer
this time asks for exactly what that stream lacked. Events must be **resumable**:
a client that reconnects with SSE's `Last-Event-ID` misses nothing that matters.

What makes up a sandbox's state is spread over the row, and each part has its
own writer (`store/DESIGN.md`, Field Ownership):

| What | Column | Only writer |
| --- | --- | --- |
| existence | `desired_state`, `state` | create, `UpdateSandbox` (service intent, the reconciler), `DeleteSandbox` |
| power | `runtime_state` | `ApplySandboxStateReports` |
| the program | `agent_status` → the primary session's `programStatus` | `UpdateSandboxAgentStatus` |

Two of those writers run constantly and almost never change anything. A pool's
complete state sync restamps every sandbox it hosts every 60 seconds, and the
agent status is pushed for every running sandbox on every poll. The server can
run on SQLite or on Postgres, and more than one server can share a Postgres
database (reconcile claims by lease). So an in-process broker cannot see every
change.

## Decision

### 1. The event

There are three event types and one control event. Every sandbox event
carries the sandbox's whole state, not a delta, so the latest event for a
sandbox is all a client needs to know about it.

```
id: 4182
event: sandbox.updated
data: {"projectId":"…","sandboxId":"…","name":"…","at":"2026-10-29T12:00:00Z",
data:  "desiredState":"present","state":"ready","runtimeState":"running",
data:  "program":{"state":"blocked","kind":"permission","app":"claude-code"}}
```

| Event | When |
| --- | --- |
| `sandbox.created` | the row is inserted |
| `sandbox.updated` | any field below changed |
| `sandbox.deleted` | the row is deleted; `data` is the last state |
| `reset` | the client's cursor is older than what is kept, or past every event there is (§4) |

The data is these fields, and nothing else:

- `desiredState`, `state`: existence, as on the sandbox (`ResourceLifecycle`).
  An archive is `desiredState: archived` and then `state: archived`.
- `runtimeState`: power (`starting`, `running`, `stopping`, `stopped`,
  `suspended`), absent before any pool has reported, and absent from the
  archive on: an archived sandbox has no container and no pool reports on it.
  The pool's next complete sync puts its last observation down, so an
  unarchived sandbox reads as not yet reported, not as it was before the
  archive, until its pool reports it again.
- `program`: the primary session's `programStatus`, reduced to `state`,
  `kind` and `app`. It is present only while `runtimeState` is `running`, since
  a stopped sandbox's last report says nothing about now. `msg`, `title` and
  `progress` are left out, because they change while the state does not.
- `name` is the sandbox's configured name, the handle it was created with, and
  not the display name a listing shows. The display name follows the title
  the program in the box sets, so carrying it would make every title change an
  event.

A write that leaves those fields as they were produces no event. Heartbeats,
the state-sync restamp, resource telemetry, pull progress and meta never do.
Adding a field is a contract change and goes in the documented schema, so the
set stays small on purpose.

### 2. Events are written with the change, by the store

The store records events in one place. A sandbox event is written inside the
transaction of the write that caused it, as a binding's events are. Each of the
writers in the table above notes the sandbox on its transaction. Just before the
outermost transaction commits, the store projects each noted row as committed,
compares that with the sandbox's latest event (`sandboxes.event_seq`), and
appends one event only on a difference. The events are recorded last so that
the counter's lock (§3) is the last lock a transaction takes. Taken in the
middle, it could deadlock against a transaction that holds a sandbox row and
is waiting for the counter. `UpdateSandboxAgentStatus` becomes a transaction to
do this. On the heartbeat path the added cost is one primary-key read, and an
insert happens only when the program's state actually moved.

Events go in `sandbox_events`: `seq` (primary key), `project_id`,
`sandbox_id`, `type`, `data`, `at`, indexed on `(project_id, seq)`.

### 3. The ID is a server-wide sequence in commit order

`seq` is taken by incrementing one counter row (`sandbox_event_log`, which
also holds §4's watermark) inside the emitting transaction. On Postgres the row lock holds the next emitter until
this one commits, so `seq` order is commit order. With an autoincrement key, a
reader could see 11 before 10 commits, and resuming from 11 would lose 10
forever. That is the gap that makes a cursor a lie. SQLite serializes writers
anyway. The lock is taken only by transactions that emit, and emitting is rare.

### 4. Retention compacts, so resuming always converges

A sandbox's latest event is kept for as long as the sandbox exists. Every
other event, `sandbox.deleted` included, is pruned once it is older than 24
hours. Each prune advances a recorded watermark, the highest pruned `seq`.

The stream is `GET /api/projects/{projectId}/events`, `text/event-stream`. It
is authorized as `list-sandboxes` is, for a person. `Last-Event-ID` is the cursor, and a
`lastEventId` query parameter is accepted for clients that cannot set a header
on their first connection. The header wins.

- **No cursor**: the latest event of every sandbox in the project, in `seq`
  order, then live. This is the current state, delivered as ordinary events
  with real IDs, so there is no separate list call to line up with the stream.
  It is read in one statement, together with every `sandbox.deleted` after the
  head read just before it. Everything committed after that statement has a
  higher `seq`, so going on from its last event misses nothing.
- **Cursor at or after the watermark**: every event after it, then live.
  Nothing has been lost.
- **Cursor before the watermark**, or past the head (a database restored from
  before it): one `reset`, with no `id`, then the same replay as no cursor.
  The client drops what it holds and takes what follows as the truth. Only
  intermediate transitions are lost, and the client is told. Because the reset
  leaves the client's cursor where it was, a client cut off before the replay
  arrives is reset again.

The stream can stand past the last event it sent: after a replay, whose events
are old, and while the project is quiet and other projects move the head. It
sends where it stands as an event with only an `id`. An `EventSource` takes
that `id` and dispatches nothing, and the client changes nothing it holds, so a
reconnect resumes from a cursor that retention has not passed. It is sent after
a replay, and while the project is quiet in place of the ping.

The server sends `retry:` once. It sends a `: ping` comment every 15 seconds
when idle, so proxies do not reap the connection. It reads new events by
polling `seq > cursor` on the indexed range every 500ms, as ADR 0081's wait
polls, so every server sharing the database serves every change. It reads the
watermark after the events, so a prune that lands in between is a reset, not a
gap.

### 5. `discobox events`

`discobox events [discobox]` streams the current project's events. It prints
one line per event. `-o json`, the CLI's one spelling for JSON, prints each
event as one line of JSON: its id, its type and its data. With a discobox
named, it shows only that sandbox's events (the filtering is client-side; the
route has no filter). It reconnects with the last ID the stream gave it, an
event's or one sent alone, so a dropped connection prints nothing twice and
skips nothing.

## Alternatives rejected

**An in-process broker publishing after commit.** This is ADR 0061's lossy
channel again. It is not resumable, and it cannot see writes made by another
server on the same database.

**Derive the stream from the rows: a change sequence stamped on `sandboxes`, and
a stream of rows whose sequence passed the cursor.** It has no event table, and
it is naturally compacted. It is rejected because deletes are hard deletes
(ADR 0010), so a deleted sandbox has no row to report. It would need a
tombstone table anyway, and the row would carry a sequence that every writer
must remember to move.

**Keep every event and refuse an old cursor with 410.** An `EventSource` stops
reconnecting on any non-200, so every browser client would need its own
recovery path. A `reset` keeps recovery inside the stream, and compaction means
recovering costs one replay, not a re-list.

**A delta per field (`sandbox.powered`, `sandbox.archived`, `program.changed`,
…).** Clients would have to fold deltas into state, and a replay after
compaction would be incomplete. Snapshots make the latest event sufficient, and
a client that wants transitions diffs two of them.

**Stream every sandbox field, or the API `Sandbox`.** That is the
`project_events` `data` blob, which nothing read (ADR 0081). It would also tie
the event contract to every future column, and any change to the resource
would become an event. A client that needs more `GET`s the sandbox.

## Consequences

- A schema migration adds `sandbox_events`, `sandboxes.event_seq`, and
  `sandbox_event_log`, which holds the counter and the watermark. The migration writes one `sandbox.updated` for
  every existing sandbox, so that a connection with no cursor covers every
  sandbox from the start.
- The store's "nothing is published on commit" rule still holds. Events are
  rows written in the transaction, never a publish.
- The heartbeat path does one more read per report, inside a transaction.
- Retention is a reconcile resource of its own (`sandboxEventPrune`), beside
  the binding expiry sweep: it arms itself for the next event due, and its scan
  is what finds the first one after a quiet spell, since nothing marks it.
- The SSE route is the server's first own streaming response. Its event schema
  is documented in `server.yaml` beside the route.
- A sandbox principal (ADR 26-10-27-880 §9) is refused this route until a
  consumer needs it scoped to the discoboxes it created.
