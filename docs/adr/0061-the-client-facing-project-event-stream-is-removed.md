# 0061 — The client-facing project event stream is removed

- **Status**: Accepted
- **Date**: 2026-08-19
- **Relates to**: [ADR 0060](0060-provisioning-progress-is-a-recorded-phase-the-client-polls.md),
  which declined to build on this stream and is why its state was examined at
  all. [ADR 0039](0039-attach-waits-for-readiness-at-every-tier.md) tier 1
  depends on the in-process broker underneath it, which this keeps.

## Context

`server/internal/projectstream` serves project resource events to clients over
two transports: a multiplexed websocket at `/projects/{projectId}/stream`, and
an OpenAPI-documented SSE route at `/projects/{projectId}/stream/sse`. Together
with the service and generated client behind them that is roughly a thousand
lines.

It was built as a Kubernetes-style list-then-watch — subscribe, receive a
snapshot of current resources bracketed by `list-start`/`list-end`, then live
changes, each stamped with a global `seq`. The watch half of that contract never
landed, and what is there does not hold together:

- **Delivery is lossy by construction.** `Broker.PublishProjectEvent` does a
  non-blocking send with a bare `default:`, so a subscriber whose 64-slot buffer
  is full silently loses events.
- **`seq` cannot be resumed from.** Every message carries one, and the
  subscription parameters are `history`, `listOnly`, and `sandboxId`. There is
  no "everything after seq N". A client can detect a gap and can do nothing
  about it, which is the one thing sequence numbers exist for.
- **One resource type is implemented.** The protocol names streams so others can
  be added; `sandbox` is the only one, three years of resources later.
- **Its only consumer is a debug command.** `disco events` prints them. Nothing
  else in the repository subscribes — the launcher polls a tick, and ADR 0039's
  attach wait uses the broker directly rather than this stream.

The combination is worse than either half. A lossy channel is a perfectly good
thing to have, and the in-process broker is exactly that: ADR 0039's tier-1 wait
treats an event as a wake-up, re-reads authoritative state on every wake, and
keeps a recheck timer *because* it knows events are dropped. What does not work
is a lossy channel dressed as a reliable one. The list-then-watch framing tells a
client it can trust the sequence, and the implementation cannot honor it.

ADR 0060 hit this directly: a status line built on the stream would stick on a
stale phase forever whenever the dropped event was the last one. It polls
instead.

There is one prospective consumer. `docs/plans/obot-integration/08-managed-command-policy.md`
has an upstream controller subscribing to project events to accelerate
reconciliation — and states its own terms: "Polling remains the correctness
mechanism... events are enqueue hints, and upstream re-reads before believing
anything." That consumer wants the broker's semantics, not the listwatch's, and
does not exist yet.

## Decision

**The client-facing project event stream is removed: both transports, the
service behind them, the generated client, and `disco events`. The in-process
broker stays.**

The line is between the two, and it is the line between a mechanism that works
and a contract that does not:

- `server/internal/events.Broker` is a lossy in-process hint channel and is
  load-bearing. ADR 0039's attach wait subscribes to it, and that wait is what
  makes an attach on a provisioning sandbox succeed instead of failing.
- Everything above it promised a reliable, resumable, list-then-watch view of
  project resources and delivered a partial one. Deleting it is cheaper than
  finishing it, and finishing it should be driven by a consumer's actual
  requirements rather than by the half already written.

Project events are still recorded and still published to the broker: the write
path is unchanged, because it is what gives the broker its payload.

### When something needs this back

Build it for the consumer that needs it, to that consumer's contract. The obot
integration is the likely one, and what it describes is a hint stream — no
snapshot, no resumable cursor, re-read before believing — which is a materially
smaller thing than what is being removed and should not inherit this protocol's
shape by default.

## Alternatives rejected

**Demote it in place.** Keep the routes, drop `history`, `list-start`/`list-end`,
and `seq`, and document it honestly as a lossy hint stream. This was the
preferred option right up until the consumer count came out at zero. Demoting
means keeping two transports, a service, a generated client, and their tests
alive for a debug command, and pre-committing the eventual consumer to a
subscription protocol chosen before it existed.

**Finish the listwatch.** Give the broker durable per-subscriber queues and add
resume-from-seq, so the sequence numbers mean what they say. Rejected because
nothing has asked for reliable delivery; the one candidate consumer explicitly
polls for correctness. Building the expensive half of a contract on speculation
is how this package got here.

**Keep it as-is.** It compiles and its tests pass, so it costs nothing to leave.
Rejected because it is not inert: it is a documented API surface that reads as a
guarantee, and ADR 0060 is the second time a design has had to stop and work out
that it cannot be relied on. A wrong map is worse than no map.

**Also stop persisting project events.** Once the stream is gone, no reader of
the `project_events` table remains, and nothing prunes it — every resource
mutation writes a row that will never be read. Rejected *for this change* rather
than on the merits: it is a schema change needing a migration and a decision
about whether the history is wanted as an audit log, and it does not belong in a
deletion. It should be settled on its own.

## Consequences

- `disco events` is gone. It was the only way to observe project events from
  outside the process, so debugging a broker problem now means a log line or a
  test rather than a command.
- `GET /projects/{projectId}/stream` and `/projects/{projectId}/stream/sse`
  disappear from the OpenAPI document, along with the four resource-event
  schemas that only they used. Any out-of-repo client of those routes breaks;
  none is known.
- `services.ProjectEventService` and `internal/resources/events` go with them,
  and the `Events` field leaves `services.Services`. The store's event-listing
  and snapshot queries lose their last callers.
- ADR 0039's attach wait is untouched, and so is every publish site.
- `project_events` rows are still written and now never read. That is a known
  loose end, recorded above and deliberately left for its own change.
