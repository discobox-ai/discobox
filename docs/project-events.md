# Project events

A project's events are a server-sent event (SSE) stream of changes to its
discoboxes, for monitoring: when a box is created, archived or deleted, when its
power changes, and when its agent starts working or needs a person. The design
and its alternatives are in
[ADR 26-10-30-868](adr/26-10-30-868-a-projects-events-are-a-retained-log-streamed-over-sse-and-sandbox-changes-are-the-first.md).

```bash
discobox events                 # the last 100 events, then live
discobox events --history 0     # live only
discobox events -o json         # one JSON object a line
```

## The stream

`GET /projects/{projectId}/events` answers `text/event-stream`.

| Request | |
| --- | --- |
| `Last-Event-ID` header | The `id` of the last event read. The stream continues after it. |
| `history` query | With no usable `Last-Event-ID`, how many past events to replay first: 0 to 1000, default 100. |

Each event is a frame with an `id`, an `event` type and one line of JSON
`data`:

```text
retry: 2000
id: 1041

id: 1042
event: sandbox.updated
data: {"sandboxId":"sbx_…","projectId":"prj_…","name":"alpha","desiredState":"present","state":"ready","runtimeState":"running","agent":{"state":"blocked","kind":"permission"},"at":"2026-10-30T12:00:00Z"}

: keepalive
```

- The first frame is `retry` and the `id` the stream starts after. It carries
  no event, but it sets the client's last event ID, so a client that drops
  before reading any event resumes from there.
- A comment (`: keepalive`) is sent after 15 idle seconds, usually with an
  `id` of where the stream now stands, so a client watching a quiet project
  resumes from there rather than from when it connected.
- IDs ascend. They are not contiguous within a project.

## Resuming

Reconnect with the last `id` read as `Last-Event-ID` (a browser's
`EventSource` does this itself) and the stream continues after it. Nothing is
missed or repeated, whichever server node answers.

Events are kept for 24 hours. If the `Last-Event-ID` is older than that, or is
one the server never gave out, the stream first sends a `reset` event with no
`id`, then starts as if no ID had been sent, with `history` events:

```text
event: reset
data: {"reason":"expired"}
```

`reason` is `expired` (retention removed it) or `unknown`. After a reset, list
the discoboxes again if you need their full state.

A server ends a stream whose client reads too slowly to keep up. Reconnecting
with the last `id` loses nothing.

## Event types

| `event` | When |
| --- | --- |
| `sandbox.created` | A discobox was made. |
| `sandbox.updated` | Its `name`, `desiredState`, `state`, `runtimeState`, `agent` or `errorReason` changed. Archiving and restoring are updates: `desiredState` and `state` say `archived`. |
| `sandbox.deleted` | It is gone. The data is its last state. |
| `reset` | The stream could not resume (above). |

An event is sent only when one of the fields below changes, so the stream stays
quiet while nothing a monitor watches moves. More kinds of event may be added,
each with its own `<resource>.` prefix. **Ignore a type you do not know.**

### `SandboxEvent`

The data of every `sandbox.*` event.

| Field | |
| --- | --- |
| `sandboxId`, `projectId`, `name` | Which discobox. |
| `desiredState` | `present`, `archived` or `deleted`: what was asked for. |
| `state` | `pending`, `awaiting_source`, `registering`, `ready`, `active`, `offline`, `archived`, `deleted` or `failed`: what the server has made of it. |
| `runtimeState` | Power: `starting`, `running`, `stopping`, `stopped` or `suspended`. Absent until its pool has reported. |
| `agent` | What the program in its primary terminal last reported of itself: `{"state": …, "kind": …}`, with `state` one of `idle`, `working`, `done`, `blocked` or `error`, and, when blocked, `kind` one of `permission`, `question` or `auth`. `null` when it reports nothing, and whenever the discobox is not running. |
| `errorReason` | Why it failed, when something classifies it: `image_unavailable`. Absent otherwise. |
| `at` | When the change was recorded. |

The OpenAPI document (`api/openapi/server.yaml`, operation
`stream-project-events`) has the same schemas: `SandboxEvent`,
`SandboxEventAgent` and `ProjectEventReset`.
