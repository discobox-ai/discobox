# 0029 — Sandboxes have no per-sandbox resource requests

- **Status**: Accepted
- **Date**: 2026-08-08
- **Supersedes**: [ADR 0003](0003-promote-pool-to-a-first-class-primitive.md)
  §4's "apply per-sandbox limits inside the pool envelope". Everything else in
  ADR 0003 stands. Completes [ADR 0012](0012-sandbox-config-is-three-attribute-owned-layers.md)
  §5's `RuntimeLayer.Resources` collapse: rather than merging
  `CPUCores`/`MemoryMB`/`DiskMB`/`TimeoutSeconds` into one representation,
  this ADR deletes `Resources` (and its `ResourceConfig`/`PoolSandboxResources`
  mirrors) outright, since every field in it turned out to be dead weight.

## Context

ADR 0003 gave every pool an overcommitted CPU/memory envelope, and paired it
with a per-sandbox request: `Sandbox.CPUVCPUs`/`MemoryBytes`, scheduled
against the pool's reported available capacity
(`Store.SchedulablePoolForSandbox`) and applied by pool-agent as a *nested*
Docker resource limit inside the pool's worker container
(`pool-agent/sandboxruntime.Runtime`). A third field, `Sandbox.StorageBytes`,
followed the same request shape.

In practice none of the three buys anything a fleet of coding-agent sandboxes
needs. Sandboxes are not being bin-packed for a size-sensitive workload — they
are interactive coding sessions whose resource use is bursty and unpredictable
at create time, and the product goal (stated when pools were introduced) is
that a pool *is* the shared, overcommitted unit; individual sandboxes were
never meant to reserve a slice of it. The fields mostly get left at their
defaults, and where set, nothing depends on the numbers being accurate:

- **CPU/memory** fed a capacity check (`pool.AvailableCPUVCPUs < cpuVCPUs`)
  and a *real* nested Docker limit applied to the sandbox container — the one
  place either field had teeth. Both are gone as of this ADR's first version.
- **Storage** turned out weaker still: `Sandbox.StorageBytes` only ever fed
  the same kind of capacity check (`pool.AvailableStorageBytes <
  storageBytes`) at schedule time — a one-shot admission comparison against
  the pool's host-measured free disk, never decremented or tracked
  afterward. Unlike CPU/memory, it was never applied as an actual per-sandbox
  disk quota anywhere: no Docker `--storage-opt`, no cgroup, no loopback-file
  quota, nothing. A sandbox that undershoots its declared request and one
  that wildly overshoots it are indistinguishable to the runtime; the number
  only ever gated whether the sandbox was allowed to be scheduled, not what
  it was allowed to consume once running.
- A parallel pipeline shadowed CPU/memory/storage the same way, and turned out
  fully dead even where the fields it mirrored had a real effect:
  `sandbox.ResourceConfig` / `PoolSandboxResources` / `sandboxconfig.Resources`.
  The server never populated `CreateOptions.Resources` in the live code path
  (only test fixtures did), so nothing in it was ever sent to pool-agent;
  the guard that would forward it (`if opts.Resources != (sandbox.ResourceConfig{})`)
  was consequently always false. Its last remaining field,
  `TimeoutSeconds`, was no exception: even where pool-agent code existed to
  receive a resources payload, it only copied the value into the sandbox's
  `sandbox.json` document, which sandbox-agent never reads to enforce
  anything — no context deadline, no process kill, no exec timeout. It had no
  CLI flag, no public API field, and no reader anywhere in the runtime.

Keeping any of these fields also kept duplicate representations alive per ADR
0012 §5's own accounting: `SandboxManifest.{CPUVCPUs,MemoryBytes}` and (before
this revision) `StorageBytes` on the API side, mirrored by
`sandboxconfig.Resources.{CPUCores,MemoryMB,DiskMB,TimeoutSeconds}` on the
runtime side — a collapse ADR 0012 already flagged as unfinished cleanup, not
a design this ADR is inventing new objections to.

## Decision

Sandboxes carry no CPU/memory/storage request. A sandbox's compute and disk
are whatever share of its pool's worker container and volumes the host gives
it, contended for like any other process sharing that container — the same
sharing ADR 0003 already established for the pool's cache and kernel.

1. **The API drops `cpuVcpus`/`memoryBytes`/`storageBytes` from sandbox
   create/read schemas** (`SandboxConfig`, `SandboxCreateConfig` in
   `api/openapi/server.yaml`; the pool-agent-internal `SandboxConfig`,
   `SandboxUpdateConfig` in `pool-agent/api/openapi/pool.yaml`), and drops
   `PoolSandboxResources` entirely, including its last field
   `timeoutSeconds` and every reference to it (on
   `PoolSandboxCreateRequest`, `PoolSandboxInstance`,
   `PoolSandboxUpdateRequest`).
2. **`Store.SchedulablePoolForSandbox` drops all capacity gating.** A pool is
   schedulable for a sandbox based purely on its lifecycle flags
   (ready/schedulable/not revoked/desired-state-present). It never refuses a
   sandbox for low free CPU, memory, or storage — that is the literal meaning
   of "no reserved capacity, shared pool usage." A pool that is genuinely out
   of disk fails the way any full disk fails a write, the same way genuine
   CPU/memory contention shows up as slowness rather than a pre-flight
   rejection; that failure mode is honest about the sharing model in a way a
   one-shot admission check at a stale snapshot of "available" never was.
3. **Pool-agent applies no nested Docker resource limit to a sandbox
   container.** The worker container's own limit (the pool envelope,
   unchanged, ADR 0003 §1) is the only cgroup boundary; every sandbox
   container inside it shares that cgroup and filesystem with its siblings.
4. **`sandboxconfig.Resources`, `sandbox.ResourceConfig`, and
   `PoolSandboxResources` are deleted outright**, along with every field on
   them (`RuntimeLayer.Resources`, `Config.Resources`,
   `CreateOptions.Resources`). This completes ADR 0012 §5's collapse: there
   is no representation left to merge, because nothing in it did anything.

The pool envelope itself (`Pool.CPUVCPUs`/`MemoryBytes`/`StorageBytes`, ADR
0003 §1, applied as the worker container/VM limit and reported host-measured
free space) is unchanged: it still sizes and reports on the shared resource
sandboxes in a pool contend over. What this ADR removes is only the
per-sandbox slice of that envelope, for all three resources.

## Rejected

- **Keep the request fields as optional/advisory hints, unenforced.** A
  request nobody schedules against or enforces is dead weight in the schema
  and a false signal to API callers that setting it does something. If a
  future use case needs sandbox-level QoS hints (e.g. a "priority" concept
  distinct from a reserved slice), it should be designed and named for that
  purpose then, not resurrected from these fields' shape.
- **Keep the storage capacity check alone, having already dropped CPU/memory
  in this ADR's first version.** Storage's gate looked more defensible than
  CPU/memory's at first glance — running out of disk is a harder failure than
  running short of CPU — but the check never did more than compare a
  declared number against a stale snapshot of free space at schedule time; it
  neither reserved space nor caught a sandbox that grew far past what it
  declared. A gate that only catches sandboxes honest enough to declare a
  large number, and never catches the actual overcommit failure mode, is not
  a safety mechanism worth keeping the field for.
- **Keep the capacity checks in `SchedulablePoolForSandbox` but always pass
  zero requests.** Equivalent in behavior to removing the checks, but leaves
  dead comparison logic and a misleading doc comment implying capacity-aware
  placement that no longer happens. Removing the checks states the actual
  policy.
- **Keep `TimeoutSeconds` alone as a hook for a future real timeout
  feature.** Considered, since it is the one field in the `Resources` family
  that is not conceptually a resource reservation. Rejected because it is
  exactly as dead as the fields it would be kept alongside — no code path
  sets it, sends it, or reads it to enforce anything — and keeping an unused
  field "for later" is the same mistake this ADR is correcting elsewhere. A
  future exec/session timeout should be designed and wired end-to-end when
  it is actually needed, not resurrected from a field that was never
  connected to anything.

## Consequences

- A pool with sandboxes already running can be scheduled onto further
  without regard to how much CPU, memory, or storage those sandboxes are
  using — pools are the unit that must be sized (or scaled out, once ADR
  0003's deferred multi-worker pools land) to keep up with sandbox load, not
  individual sandbox requests.
- `Pool.AvailableCPUVCPUs`/`AvailableMemoryBytes`/`AvailableStorageBytes`
  (agent-reported, ADR 0003) stay: they remain useful for pool-level
  observability (`disco pool get`/`list`) even though nothing gates
  scheduling on them anymore.
- Existing databases keep stale `cpu_vcpus`/`memory_bytes`/`storage_bytes`
  columns on `sandboxes` only until the accompanying migration drops them;
  the same-named columns on `pools` are the envelope and are untouched.
- `harness.Configure.{CPUVCPUs,MemoryBytes,StorageBytes}` (already-dead
  fields no driver sets and nothing reads) are left alone — out of scope,
  unrelated to the live per-sandbox request path this ADR removes.
- Any future need for a per-sandbox timeout, priority, or QoS concept starts
  from a clean slate: no field, schema, or struct survives from this ADR for
  it to be grafted onto.
