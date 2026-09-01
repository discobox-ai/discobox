# 0081 — Project events are not persisted, and the attach wait polls

- **Status**: Accepted
- **Date**: 2026-09-01
- **Settles**: [ADR 0061](0061-the-client-facing-project-event-stream-is-removed.md),
  which removed the client-facing stream and deferred this: "no reader of the
  `project_events` table remains, and nothing prunes it... It should be settled
  on its own."
- **Relates to**: [ADR 0039](0039-attach-waits-for-readiness-at-every-tier.md),
  whose tier 1 was the broker's only consumer and is what this rebuilds.

## Context

ADR 0061 left the write path in place because it was what gave the in-process
broker its payload. What that left behind is a table with one writer and no
reader at all:

- Nothing issues a `SELECT` against `project_events`. The only statement besides
  `INSERT` is the cascade in `DeleteProject`.
- The single consumer of an event is `AwaitSandboxHTTPClient`, and it reads
  three fields — project, resource type, resource ID — from the in-memory copy.
  The `data` blob, a full JSON serialization of the mutated resource, is the
  bulk of every row and is read by nothing.
- Nothing prunes. Rows die only with their whole project, so a long-lived
  project accumulates events for sandboxes deleted months ago.
- The table carries six indexes, all maintained on every insert, none ever used
  by a query.

The write rate is not incidental either. Three sibling paths made three
different decisions about the same kind of high-frequency observation:
`RecordPoolProvisionProgress` publishes nothing and says why; the sandbox state
report publishes only when the state actually moved; and
`ApplySandboxProgressReports` writes a row for every report it receives, with no
change check. Pull progress is throttled at 500ms, so one sandbox pulling an
image inserts two full-resource rows a second for the length of the pull.

That leaves the broker. It is a genuine mechanism — but only a latency
optimization, and its own design says so. ADR 0061 describes it as "a lossy
in-process hint channel"; the wait re-reads authoritative state on every wake
and keeps a 15-second recheck timer *because* events are dropped. The
subscription costs a package, a publisher interface on the store, an
after-commit event buffer threaded through every transaction, a generic
`withResourceEvent` wrapper around twenty mutations, and three `Event*()`
methods on nine models — to save, at most, the recheck interval.

## Decision

**`project_events` is dropped, along with the model, the store's event
machinery, and `server/internal/events`. The attach wait polls the rows it
already re-reads.**

The wait keeps its shape from ADR 0039 — attempt, decide whether waiting can
help, wait, retry — and changes only what it waits on:

- Between attempts it sleeps `sandboxReachablePollInterval` (500ms) instead of
  selecting on a subscription.
- Progress, which restarts the stall budget, is a change in the `updated_at` of
  the sandbox row and of the pool hosting it, rather than the arrival of an
  event naming either.

Polling is not a downgrade here; on three counts it is the better signal:

- **It sees progress the events never carried.** `RecordPoolProvisionProgress`
  and `RecordPoolImageStage` deliberately publish no event, so today a pool host
  pulling a multi-gigabyte image stalls the wait at two minutes while visibly
  making progress. Both write the pool row, so both now count.
- **It ignores progress that was not progress.** An unconditional progress
  report refreshed the stall budget whether or not anything changed. `updated_at`
  moves when a write actually happened.
- **It has no subscription window.** The current code takes the subscription
  before the first attempt specifically so a transition cannot land between a
  failed acquire and the subscribe. A poll re-reads unconditionally and has
  nowhere for that race to live.

The cost is bounded and small. An acquire attempt is entirely local — two
indexed reads and a pooled client lease, no network — and a wait is capped by
the same two-minute stall budget it always had. Against that, every resource
mutation in the system loses an insert plus six index updates, and the progress
report path — the loudest writer — loses two of those per second per pulling
sandbox. Total database work goes down.

## Alternatives rejected

**Drop the table, keep the broker.** Publish a `{projectID, resourceType,
resourceID}` value after commit and write no row. This removes all of the
storage cost and none of the plumbing: the publisher interface, the transaction
event buffer, `withResourceEvent`, and twenty-seven model methods all stay, to
deliver a hint that a 500ms poll of a row already in the working set delivers as
well. It is the smaller diff and the worse structure — an abstraction kept
because it exists rather than because something needs it.

**Keep persisting as an audit log.** Defensible, but it is a different feature
with different requirements: retention and pruning, a decision about what is
worth recording, and a change check on the progress path so a byte counter stays
out of the audit trail. Nothing has asked for it. Build it when something does,
to that consumer's requirements — the same conclusion ADR 0061 reached about the
stream itself.

**Prune on a schedule and leave the rest alone.** Caps the growth and keeps
every other cost: the insert per mutation, the six index updates, the plumbing,
and a table nothing reads. Adding a reaper to maintain data with no reader is
work spent to keep a thing that should not exist.

**Poll only the sandbox row, not the pool.** One less read per iteration. But
the pool is half of what the wait is watching — `ErrSandboxPoolNotReachable` is
a refusal only the pool row can clear, and while a pool is coming up the sandbox
row does not change at all. Dropping it would reintroduce, as a stall, exactly
the case the pool-progress point above fixes.

## Consequences

- `server/internal/events` is gone, as are `store.EventPublisher`,
  `store.WithPublisher`, the store's after-commit event buffer,
  `withResourceEvent`/`createProjectEvent`, `model.ProjectEvent`, the
  `EventType*`/`EventAction*` constants, and the `EventProjectID`/
  `EventResourceType`/`EventResourceID` methods on nine models.
- Store mutations that were wrapped in a transaction solely to write their event
  atomically become plain writes. Transactions remain wherever two or more
  statements need to commit together.
- The `project_events` table is dropped by migration. Its history is discarded;
  nothing had read it since ADR 0061, and the ADR 0010 hard-delete rule already
  means the repository keeps no tombstones.
- An attach that is waiting adds up to `sandboxReachablePollInterval` of latency
  once the gate opens, in exchange for the wait noticing pool-side progress it
  previously could not see.
- `service.New`'s variadic broker parameter and
  `sandboxes.Service.SetEventBroker` are removed; the sandbox service reaches the
  store directly, which is what the wait was doing through the broker anyway.
