# 0003 — Promote pool to a first-class primitive

- **Status**: Accepted
- **Date**: 2026-07-17

## Context

Sandboxes that share a worker share a kernel, a filesystem, a cache, and an
overcommitted CPU/memory envelope. That sharing is the point — a fleet of
coding-agent sandboxes wants a common build/dependency cache and wants to
overprovision against one resource envelope — and it is also a security
boundary the user must be able to see and control. Today that boundary is an
accident of placement: `Worker` is an internal implementation detail of
worker-backed providers, invisible in the product model, and the only grouping
above it is "provider instance = one homogeneous pool"
(`server/providers/DESIGN.md`), which conflates backend credentials/config with
capacity and sharing policy.

Two pressures force the boundary into the model now:

1. **Kubernetes support.** The planned k8s backend runs each pool's worker as a
   pod (worker-agent + nested runtime + pool volume). Kubernetes schedules
   pools; discobox schedules sandboxes into pools. A shared writable cache and
   a shared overcommit envelope cannot be expressed per-sandbox-pod in k8s;
   the pool is the unit both schedulers agree on.
2. **Security legibility.** "What shares a cache and kernel with my sandbox"
   must be a declared, user-visible fact — later RBAC-controlled — not an
   emergent property of scheduling.

This is a new system: no backwards compatibility is owed. Primitives and
relationships may change freely.

## Decision

### 1. `Pool` is a top-level, project-scoped resource

- New model/API/CLI resource `Pool` (`pool_` ID prefix in `id/id.go`), created
  by users, RBAC-gated later. Project-scoped like every other asset; there are
  no global pools.
- A pool binds to exactly one **provider instance at create time, immutable
  after**. The provider instance is reduced to backend identity: type,
  credentials, connection config. Everything about capacity moves to the pool.
- Pool attributes:
  - **Envelope**: total CPU vCPUs, memory bytes, storage bytes available to
    the pool. Sandbox resource requests are scheduled against the envelope and
    may overcommit it per pool policy.
  - **Cache**: shared cache volume config, mounted into every sandbox in the
    pool at a well-known path.
  - **Isolation/capability profile** (hook for ADR-0004): userns/rootless
    settings, nested-runtime capability flags. Attributes on the pool because
    the pool is the boundary they describe.
- Pool semantics are the product contract: sandboxes in the same pool share a
  cache, share the envelope, and share a weaker isolation boundary (same
  kernel/host). Cross-tenant or mutually untrusted work belongs in different
  pools.

### 2. Sandboxes are assigned to a pool, not a provider

- `Sandbox.PoolID` is required, resolved at create, immutable after.
- `Sandbox.ProviderInstanceID` is removed; provider resolution is
  sandbox → pool → provider instance.
- `Project.DefaultSandboxProviderID` is replaced by `Project.DefaultPoolID`.
  Each project gets a default pool (auto-created against the project's
  built-in provider instance on first use), so `disco run` keeps working with
  zero configuration.

### 3. Worker becomes the runtime host of a pool

- `Worker.ProviderInstanceID` is replaced by `Worker.PoolID`; workers remain
  internal (operator-visible, not part of the user contract). Scheduling
  (`FindSchedulableWorker`) keys off the pool.
- Worker-pool sizing/capacity policy (`workerpool.WorkerPoolConfig`) moves
  from provider configuration to pool attributes. The provider instance no
  longer implies a pool.
- Initially **one worker serves one pool** at a time: a shared writable cache
  requires one host, and the envelope equals the worker size. Worker identity
  stays replaceable underneath the pool (repair/replace preserves the pool and
  its volumes), which is exactly why the user contract binds to the pool and
  never to the worker.
- Provider status summaries derived from worker rows
  (`SandboxProviderInstanceStatus`) become pool status.

### 4. Worker-agent enforces pool semantics

- Mount the pool cache volume into every sandbox.
- Apply per-sandbox limits inside the pool envelope; the envelope itself is the
  worker container/VM/pod limit, so overcommit falls out of the runtime
  hierarchy rather than scheduler arithmetic.

## Rejected

- **Keep provider instance as the implicit pool.** Cannot express multiple
  pools per backend, hides the sharing boundary from users, and welds capacity
  policy to backend credentials. The k8s backend makes this untenable: one
  cluster (one provider instance) must host many pools with different sharing
  and isolation profiles.
- **Expose `Worker` as the user-facing boundary.** Workers are replaceable
  runtime hosts — repair swaps the container/VM while identity and volumes
  survive. A user contract pinned to a worker breaks on every repair, and on
  k8s the "worker" is a pod the user should never see. The boundary users
  reason about must survive worker replacement; hence a separate resource.
- **Skip the primitive; let k8s pods be sandboxes and its scheduler place
  them.** Rejected for two independent reasons: kube-scheduler has no concept
  of a shared overcommit envelope with a shared writable cache (same-node
  affinity plus burstable QoS reconstructs the pool badly), and it forks the
  runtime path (source delivery, manifest, proxy, sandbox API) into a second
  implementation that must stay functionally in sync with the docker/VM path.
- **Global or cross-project pools.** Everything in discobox is project-scoped;
  a sharing boundary that crosses project tenancy would undermine the security
  story the pool exists to make legible.

## Deferred

- **Multi-worker pools** (envelope split across hosts, network-backed shared
  cache). The `Pool` contract deliberately does not promise one host so this
  can arrive without a model change. Revisit when a pool's envelope exceeds
  what one worker host can provide, or when a shared cache backend that spans
  hosts is adopted.
- **Pool RBAC.** Create/assign is open within a project until project roles
  grow finer-grained permissions.
- **Pool drain semantics.** Pool delete initially requires the pool to be
  empty of sandboxes; draining/migration is deferred until sandboxes can move
  between pools, which today they cannot (pool assignment is immutable).
