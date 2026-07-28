# WI-06 — Pool suspension, envelope enforcement, and overcommit placement

**Goal:** make a pool an administrator-controlled capacity envelope that
sandboxes share by overcommit, with an explicit suspension switch, rather than a
capacity pool that admits sandboxes by instantaneous available-resource checks.

Read `00-CONTEXT.md` first. **The overcommit decision is blocked on WI-01;**
the suspension work is not and can start immediately.

## Why

Upstream models a pool as one user's machine: all of that user's agents share
its CPU, memory, storage, and cache. Starting another agent does not reserve a
slice and is not rejected because the sum of per-sandbox requests exceeds the
envelope. As contention grows, CPU is shared and agents slow down, memory
pressure may terminate processes, storage pressure may fail operations — and the
pool reports health, pressure, and usage. Discobox owns QoS and termination
policy inside the pool; upstream does no bin packing and holds no reservations.

Separately, an administrator needs to stop *new* work on a pool without
destroying it or stopping what is already running. There is no way to express
that today.

## Current state

**Placement is admission-by-available-capacity.**
`Store.SchedulablePoolForSandbox` (`server/internal/store/pools.go:275-307`)
refuses placement unless the pool is unrevoked, active, ready, schedulable, and

```go
pool.AvailableCPUVCPUs     >= sandbox.CPUVCPUs &&
pool.AvailableMemoryBytes  >= sandbox.MemoryBytes &&
pool.AvailableStorageBytes >= sandbox.StorageBytes
```

Those `Available*` fields are *instantaneous agent-reported* values
(`model.go:506-508`), written by pool heartbeats through
`server/internal/resources/pools/agent_service.go:60`. So placement today
depends on a sampled number that moves under the caller's feet — which is the
worst of both worlds: it is not a real reservation, but it does reject work.

**There is no suspension.** `model.Pool.Schedulable` (`model.go:504`) is
agent-reported, not administrator-declared, and is written by the heartbeat and
by the reconciler on failure paths (`resources/pools/reconciler.go:224,253`).
Overloading it for administrative suspension would let the next heartbeat
silently clear the administrator's intent.

**The envelope is enforced only partly, and one third of it is inert.**
`Pool.CPUVCPUs`, `MemoryBytes`, `StorageBytes` (`model.go:496-498`) are
documented as the total capacity the pool's sandboxes may overcommit, with zero
meaning "sized by the host". In practice the Docker pool host applies
`pool.CPUVCPUs` -> `NanoCPUs` and `pool.MemoryBytes` -> `Memory`
(`server/providers/dockerworker/engine.go:401-411`), and **`pool.StorageBytes`
is read by nothing outside CLI display** — it is accepted, persisted, shown, and
never enforced. Do not leave it looking implemented; either enforce it or
document it as unenforced.

**Changing a pool's envelope today force-recreates the pool host container.**
The envelope is compared via a container *label*, so any capacity change
destroys and recreates the host rather than updating it — an outage, not the
"applies immediately, increases contention" behavior this item wants. The fix
is to compare against the live `HostConfig` and apply changes with
`ContainerUpdate`. Note this changes pool host lifecycle behavior for *every*
pool, not just managed ones, so confirm the blast radius with the engineer
before building it.

**The placement gate fires only on sandbox create.**
`SchedulablePoolForSandbox` is reached solely from `poolruntime.Provider.Create`.
`Start` and `Restart` perform no pool check at all — they talk straight to the
agent. So suspension cannot be enforced in the store gate alone; it needs
service-layer enforcement at the start/restart entrypoints too.

**A failed placement is terminal, with no wake-up path.** The sandbox reconciler
calls the non-retryable `FailOperation`, and `ScanDirty` explicitly excludes
terminal failures. Nothing marks sandboxes dirty when a pool becomes healthy
again. "Clearing suspension resumes sandboxes" therefore needs real re-drive
intent, not just a dirty mark.

**`Schedulable` is rewritten on every heartbeat** (`store/pools.go:257`) and by
`RegisterPool` (`store/pools.go:219`), as well as by the reconciler's failure
paths. This is confirmed, not suspected: it cannot carry administrator intent,
and a separate `Suspended` column is the right call.

The findings above came out of an aborted implementation run. They are
observations about existing behavior, not decisions — verify before relying on
them.

Relevant accepted ADRs: `0003-promote-pool-to-a-first-class-primitive.md`,
`0006-pool-is-the-runtime-host.md`,
`0013-local-linux-pools-use-libkrun-microvms.md`.

## Scope

1. **Suspension.** A desired-state `Suspended` field on `Pool`, distinct from
   agent-reported `Schedulable`, settable through the pool API and never
   overwritten by a heartbeat. Semantics: blocks new sandbox placement and
   start/restart operations; does **not** stop already-running sandboxes. An
   administrator may stop a sandbox while suspended, and it stays stopped.
   Clearing suspension resumes reconciliation and starts sandboxes whose desired
   state is running.
2. **Placement.** Per WI-01's decision, remove the instantaneous available-
   capacity comparison from the placement gate — for managed pools at minimum,
   possibly for all pools. What remains: the pool must be unrevoked, active,
   ready, schedulable, and not suspended. Do not replace one admission rule with
   another.
3. **Envelope enforcement.** The configured capacity is the pool runtime's outer
   boundary, applied at the host, not an admission check at the API. Establish
   what the pool host enforces today and close the gap.
4. **Envelope reduction.** Reducing a live pool's envelope applies immediately
   and may increase contention or trigger QoS/termination. That is intended
   behavior, but it should be a deliberate, tested path rather than an
   accident.
5. Update `server/internal/resources/pools/DESIGN.md` and the store `DESIGN.md`
   to describe the placement gate's new meaning.

## Out of scope

- Reporting pressure, usage, QoS actions, or termination reasons — WI-07.
- Managed-pool routes and external identity — WI-03.
- Per-sandbox resource guarantees. Per-sandbox CPU/memory/storage values stay
  *observations and hints*, never allocations. Do not turn them into requests.

## Design questions for the engineer

- **Managed-only or universal overcommit?** WI-01 should have settled it. If
  not, settle it there first — this item implements, it does not decide.
- **Do per-sandbox `cpuVcpus`/`memoryBytes`/`storageBytes` still mean anything**
  once they no longer gate placement? They may still shape cgroup limits inside
  the pool. If they become purely advisory, say so in the API documentation.
- **What happens to a sandbox whose requested storage exceeds the whole
  envelope?** Overcommit says do not reject; physics says it will fail. Failing
  at runtime with a clear condition is probably right, but confirm.
- **Should suspension be a desired-state field or a separate lifecycle state?**
  A field composes better with the existing `desiredState` enum
  (`active`/`deleted`) than a third enum value would.

## Done when

- A suspended pool refuses new sandboxes and start/restart, leaves running
  sandboxes alone, and resumes correctly when unsuspended — and a heartbeat
  cannot clear the suspension.
- Many sandboxes can be launched into a pool beyond its nominal capacity without
  admission rejection.
- The envelope is enforced at the runtime boundary.
- Pool `DESIGN.md` files describe the new placement semantics.
- `go tool task check-hooks` passes.
