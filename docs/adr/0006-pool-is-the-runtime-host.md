# 0006 — Pool is the runtime host; the worker resource is removed

- **Status**: Accepted
- **Date**: 2026-07-17

## Context

ADR-0003 promoted `Pool` to the user-visible sharing boundary and kept
`Worker` as a separate internal resource: the replaceable runtime host of a
pool, with the note that initially one worker serves one pool. That
worker/pool split exists to keep two options open — multi-worker pools later,
and zero-downtime host replacement (launch a healthy worker before removing a
failed one).

Neither option is exercised. The default pool is sized to exactly one worker,
runtime recovery is in-place replacement (the engine swaps the container/VM
under the same identity while named volumes survive), and the planned k8s
backend maps a pool to a single pod. Meanwhile the indirection carries real
cost: a second orchestrated resource with its own lifecycle, worker-pool
sizing math (desired-additional-workers, excess deletion, retention priority),
settled-failure detection across workers, registration-timeout
delete-and-replace, capacity best-fit scheduling across worker candidates, and
a two-level reconcile chain (worker reconcile re-marks the pool, the pool
re-sizes workers).

## Decision

- **A pool is its own runtime host.** The `Worker` resource is deleted. The
  pool row carries the runtime lifecycle directly: desired state/phase/
  generation, registration identity and public key, ready/schedulable/degraded
  scheduling flags, reported capacity, heartbeat, conditions, and provider
  runtime state. Bootstrap tokens attach to the pool.
- **One host per pool is an invariant, not a default.** A pool resides on one
  host: a container or VM for VM-style backends, a pod on Kubernetes. Capacity
  beyond one host means creating more pools; sandboxes never span pools.
- **Scheduling is a gate, not a search.** Placing a sandbox checks that its
  pool is ready and schedulable and that the request fits the pool's reported
  capacity. There is no candidate selection.
- **Recovery is in-place.** Repairing a pool replaces its runtime under the
  same pool identity; pool-local state survives in named volumes. A brief
  outage during repair is accepted. Pool deletion is intent-based: the
  reconciler removes the runtime, then the row.
- **The worker agent becomes the pool agent.** The in-guest module, image,
  bootstrap metadata, auth claims, and worker-local API routes are renamed
  from worker to pool (`/api/project/{project_id}/pool/{pool_id}/...`); the
  agent registers with a pool identity.

## Rejected

- **Keep the worker indirection for future multi-host pools.** The layer is
  pure carrying cost today: none of its code paths run in any supported
  configuration, and internal resources are cheap to reintroduce — the Pool
  user contract still does not name a host, so adding an internal host record
  later is an implementation change, not an API break (the database is
  disposable at this stage, per project policy).
- **Keep workers for zero-downtime host replacement.** Overlapping replacement
  only matters once sandboxes must survive host swaps without interruption;
  today a failed host already interrupts its sandboxes, and in-place repair is
  the mechanism actually used by every backend.

## Consequences

- Reintroducing multi-host pools later means re-adding an internal host
  record and scheduling across hosts — a real lift, accepted knowingly.
- Pool rows mix user-configured policy (envelope, cache) with agent-reported
  runtime state; generation guards arbitrate concurrent writes.

## Deferred

- **Multi-host pools** (envelope split across hosts, network-backed shared
  cache). Revisit when a pool's envelope must exceed one host; this
  supersedes ADR-0003's multi-worker deferral with a stronger single-host
  invariant.
