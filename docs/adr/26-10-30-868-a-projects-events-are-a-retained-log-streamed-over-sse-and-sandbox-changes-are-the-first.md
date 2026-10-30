# 26-10-30-868 — A project's events are a retained log streamed over SSE, and sandbox changes are the first

- **Status**: Accepted
- **Date**: 2026-10-30
- **Relates to**:
  [0061](0061-the-client-facing-project-event-stream-is-removed.md) (the last
  stream: lossy, `seq` not resumable, removed; "build it for the consumer that
  needs it, to that consumer's contract"),
  [0081](0081-project-events-are-not-persisted-and-the-wait-polls.md) (dropped
  `project_events`; its write amplification is what §4 avoids),
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md)
  (the program status §3's agent state is read from).

## Context

Nothing outside the server can watch sandboxes change. A monitor — a
dashboard, a script waiting for a box to stop, something noticing an agent
blocked on a person — polls `list-sandboxes` and diffs. `discobox events` went
with 0061.

This is the consumer 0061 said to wait for, and its contract is the one 0061
declined to build on speculation: delivery must be **reliable and resumable**.
A client that drops its connection reconnects with the standard SSE
`Last-Event-ID` and receives exactly what it missed, from whichever server node
it reaches. The reconcile engine already runs several nodes against one
database, so an in-process buffer cannot do this; the log has to be in the
database.

The stream is the project's, not the sandboxes': other kinds of event may join
it later. It starts with one kind, sandbox changes, because that is what is
needed now. Unlike 0061, it carries a small documented payload per kind and no
list-then-watch framing.

## Decision

### 1. The route

`GET /projects/{projectId}/events`, `text/event-stream`.

- With no `Last-Event-ID`, the stream replays the project's last `history`
  events (query parameter, default 100, 0–1000), then continues live.
- With a `Last-Event-ID` the log still holds, it continues from the event after
  it. `history` is ignored.
- With a `Last-Event-ID` the log no longer holds — pruned (§6), or ahead of
  the log — it sends `reset`, then behaves as a connect with no ID.
- Every frame has `id:` (except comments and `reset`), `event:`, and one JSON
  `data:` line. The server sends `retry: 2000` once and a `:` comment every
  15 seconds while it is idle, so proxies keep the stream open.

Its visibility is the project's: a person with access to the project sees
every event in it. It is not in the sandbox role (Deferred).

### 2. One log per project, one ID space across kinds

Every event in a project, of every kind, is a row of one table,
`project_event_log`: ID, project, `type`, resource ID, JSON payload, time. One
`Last-Event-ID` therefore resumes the whole stream however many kinds it
carries, and events of different kinds keep the order they committed in.

`type` is `<resource>.<change>`. The `<resource>` prefix is a namespace a
future kind claims (`pool.*`, `job.*`), and the OpenAPI document lists every
type with its payload schema. A client ignores a type it does not know. That
is the documented forward-compatibility rule, and it is what lets a kind be
added without a new route or a breaking change.

The table is not called `project_events`. Startup drops that name whenever it
finds it (0081's retirement, `dropProjectEvents`), so a table by that name
would be wiped on every boot.

### 3. The sandbox events

Three types share one documented schema, `SandboxEvent`:

| `event:` | When |
| --- | --- |
| `sandbox.created` | The sandbox row is inserted. |
| `sandbox.updated` | Its projection (below) changed. |
| `sandbox.deleted` | The row is deleted (an archived box past retention, or an explicit delete). The payload is its last projection. |

`SandboxEvent` is the sandbox's **projection**, the state a monitor watches,
and nothing else:

- `sandboxId`, `projectId`, `name`
- `desiredState`, `state` — existence: create, ready, failed, archive, restore
- `runtimeState` — power: `starting`, `running`, `stopping`, `stopped`,
  `suspended`, or empty before any report
- `agent` — `{state, kind}` from the primary terminal's `programStatus`
  (26-10-16-424): `idle`, `working`, `done`, `blocked`, `error`, and `kind`
  when blocked; null when it reports none
- `errorReason`
- `at` — when the change was recorded

A client that wants more re-reads the sandbox. Archive is not its own event
type: it is an `updated` whose `desiredState`/`state` say `archived`, and the
docs say so.

`reset` carries `{reason}` (`expired` or `unknown`) and no `id:`. It is the
stream's own type, not a resource's, so it has no prefix.

Adding a field to the projection is a contract change, because every change to
that field becomes an event. So is adding an event kind. Either goes through
an ADR that decides its projection the way this one does, not a drive-by edit.

### 4. An event is written when the projection changes, in the writing transaction

Every store writer that can move a projected field computes the projection
before and after, inside its own transaction, and appends an event only if the
two differ: `CreateSandbox`, `UpdateSandbox`, `ApplySandboxStateReports`,
`UpdateSandboxAgentStatus`, `DeleteSandbox`. The event commits with the change
or not at all, so the log never claims a state the row does not hold and never
misses one it does.

So the loud writers stay quiet. A 60-second state restamp, a heartbeat, a pull
progress report, a resource sample, and an agent-status tick whose primary
program reports the same state write nothing. That is the write amplification
0081 removed, and it does not come back. A future kind follows the same rule.

### 5. Event IDs commit in ID order

An event's ID is a per-database counter in a one-row table, incremented under
its row lock in the writing transaction. Postgres therefore commits events in
ID order: a writer holding ID 11 cannot commit before the one holding 10
releases the lock. SQLite has a single writer, so it does this without asking.
The ID is sent as a decimal string. It is global, not per project, so a
project's IDs ascend but are not contiguous.

This is what makes resuming lossless. With a plain auto-increment key, 11 can
commit first. A reader that has read past 11 by the time 10 commits never sees
10, and no `Last-Event-ID` ever brings it back (`audit --follow` re-reads by
time windows for the same reason). The lock is taken only by a transaction
that appends an event, so it serializes real state changes, not every write.

### 6. Retention

Events older than 24 hours are pruned by a periodic sweep in the reconcile
engine. A client that was away longer gets `reset` and a fresh `history`: the
log is for resuming a dropped stream, not for audit. Every kind shares the one
retention window.

### 7. Delivery

Each node runs one tailer per database, polling `id > last` every 500ms (0081's
interval) and handing new events to that node's open streams, each filtered to
its project. A stream whose client cannot keep up is **closed, never thinned**.
Its client reconnects with its `Last-Event-ID` and loses nothing. That is the
difference from 0061's broker, whose full buffer silently dropped events.

### 8. `discobox events`

`discobox events [--history N]` streams the resolved project's events until
interrupted. It prints one line per event — time, type, then the kind's own
summary (for a sandbox: name, state, power, agent) — or, with JSON output, the
event as received. A type it does not know is printed raw in JSON and by its
type alone otherwise. It reconnects automatically with the last ID it printed.
The event types and their schemas are documented in the CLI and API docs.

## Alternatives rejected

**`/projects/{projectId}/sandboxes/events`, one stream per resource.** Each
stream would be simpler on its own. But a monitor watching two kinds would
hold two streams with two `Last-Event-ID`s and no order between them, and every
new kind would need a new route. One project stream with namespaced types
costs only the rule that a client ignores what it does not know.

**One log table per kind.** Each kind owns its storage. But resuming across
kinds would then need a composite cursor, and `Last-Event-ID` is one opaque
string. The shared table with a typed JSON payload keeps resumption trivial.

**An in-memory ring buffer per node, IDs stamped with a boot epoch.** No table
or migration. But a reconnect may reach another node, or the same node after a
restart. Either is an unknown ID and a `reset`, so it is not resumable in
exactly the cases resumption exists for.

**Derive events by diffing periodic reads of `sandboxes`.** A watcher reads
every row and emits what moved. That misses a change undone between two reads
(a stop and start inside one poll). It also needs a single leader to assign IDs
so every node agrees, and that leader is a component nothing else needs.

**An event on every row write.** Simpler to state. But the agent-status tick
and progress reports would make most events noise, and it is 0081's write
amplification again.

**A full snapshot of the resource in each event.** It would give the whole
state at the time of the change. But a sandbox runs to kilobytes of opaque
agent status and manifest, which is 0081's unread full-resource rows again, and
it would make the event contract the whole `Sandbox` schema rather than a
documented projection. Events fire on projection changes anyway, so the other
fields in a snapshot would only be incidental.

**Database triggers.** These write the log without touching store writers. But
they mean two dialects (SQLite and Postgres) and the projection (the primary
terminal's program state out of an opaque JSON blob) in SQL, and they hide
writes from the store, which owns persistence.

**A snapshot of every present sandbox on connect, then live.** This tells a
monitor everything at once. Replaying recent history was preferred: it shows
what just happened, which a snapshot cannot, and `list-sandboxes` already
serves the full state.

**Separate event types per axis (`sandbox.power`, `sandbox.agent`, `sandbox.archived`).**
These are easier to filter by eye. But one change can move two axes at once (a
report that stops the box clears its agent), and with per-axis types a client
has to stitch them back into state. One `updated` with the whole projection is
idempotent: a client can always take the latest event as the truth.

**WebSocket.** No reconnect or resume semantics are built in, and
`Last-Event-ID` is exactly the contract asked for. SSE also passes through
proxies and `curl` untouched.

## Consequences

- One new table pair (`project_event_log`, and the counter row), added by
  migration. Existing databases get an empty log: history starts at upgrade.
- The five writers in §4 gain a projection compare and a conditional insert.
  A new writer of a projected field must do the same, and a store test pins
  that each one emits and that the restamp, heartbeat and progress paths do
  not.
- Every node polls one indexed query every 500ms whether or not anyone is
  streaming. The tailer could start lazily with the first stream; that is an
  implementation choice.
- A slow consumer reconnects rather than stalling the tailer.
- A future kind adds its types, its payload schema and its writers' compares.
  The route, table, IDs, retention and tailer are unchanged.

## Deferred

- **The sandbox role.** A discobox watching the boxes it created would need
  the stream filtered to `created_by_sandbox_id`, which is a sandbox-kind
  filter. Add it when an in-box orchestrator asks, together with a decision on
  which other kinds such a caller may see.
- **A type filter on the route** (`?type=sandbox.*`). With only one kind there
  is nothing to filter. Add it with the second kind if a consumer wants only
  one of them.
- **Other kinds.** Pools and jobs are the likely next ones. Each needs its own
  projection and change rule decided the way §3 and §4 decide sandboxes'.
