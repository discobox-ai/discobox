# WI-07 — Live pool and per-sandbox utilization

> Status (checked 2026-09-11): superseded in substance.
> [ADR 0071](../../adr/0071-resource-accounting-is-a-pool-agent-differenced-report.md)
> (Accepted, implemented) shipped pool and per-sandbox resource accounting a
> different way: the pool agent pushes a differenced report every 30s, and the
> server persists it on `Pool.resources` and `Sandbox.resources` — which this
> plan rules out. Re-plan against ADR 0071 before picking this up; only what it
> does not cover (an explicit unavailable response for an offline pool,
> termination/OOM reporting, an unpersisted on-demand read) is still open here.

**Goal:** a read-only, live, point-in-time utilization snapshot for one pool and
its sandboxes, collected by the pool agent and served through the server without
being persisted.

Read `00-CONTEXT.md` first. **Fully independent — can start now, in parallel
with everything else.** It touches no contract that other items depend on.

## Why

Users and administrators need to answer "what is this pool consuming right now,
and which agent is responsible?" — especially once pools are shared by overcommit
(WI-06), where contention is the expected failure mode rather than an anomaly.

Utilization is deliberately *not* part of desired-state reconciliation. It is
high-churn measurement: keeping it out of the resource store and the project
event streams is the point. Lifecycle status stays suitable for controllers and
watches; utilization stays an on-demand diagnostic read.

## Current state

Nothing serves an unpersisted, on-demand read. The adjacent things are each a
different shape:

- **The ADR 0071 resource report.** Every 30s the pool agent posts one report
  (`POST /api/pools/{poolId}/resources`) with pool-wide usage and a per-sandbox
  breakdown, rates differenced from cumulative counters over one tick
  (`pool-agent/resourcereport.go`). The server stores it on `Pool.resources` and
  `Sandbox.resources`; `discobox admin pool resources` and a per-sandbox
  `resources` command display it. It answers "which agent is responsible", but
  it is pushed, persisted, and up to a tick stale.
- **Pool heartbeats** report coarse `availableCpuVcpus`, `availableMemoryBytes`,
  `availableStorageBytes` plus an opaque `conditions` JSON blob
  (`model.Pool`, written via
  `server/internal/resources/pools/agent_service.go`). That is remaining
  headroom, not a utilization breakdown, and it has no per-sandbox dimension.
- **`SandboxRuntime`** (`api/openapi/server.yaml`) carries lifecycle — desired,
  display, and runtime state, generations, error message — plus agent status
  and, since ADR 0071, the stored `resources` report.
- **Sandbox-agent resource samples** (`sandbox-agent/resources/collector.go`,
  stored by `sandbox-agent/store`) dump cgroup and procfs data as opaque JSON
  scoped to an individual *exec*, keyed by terminal ID, retained by count
  (`ResourceRetentionCount: 300`, set in
  `pool-agent/sandboxruntime/runtime.go`). Per-exec, not per-sandbox; opaque,
  not typed; and inside the sandbox rather than above it. The server proxies them per exec
  (`.../execs/{execId}/resources`, `/history`, `/stream`).

The transport you need already exists: `poolruntime.RuntimeProvider.AcquirePoolAgentClient`
(`server/providers/poolruntime/provider.go`) returns an authenticated HTTP
client lease to a pool's agent, used today by `server/providers/poolruntime/agent_client.go`.

## Scope

1. **Collector in the pool agent.** The pool agent is the right collector: it
   owns the sandbox runtimes and can inspect their cgroups/container state
   independently of the outer provider, and it can produce pool and sandbox
   figures on a common sample boundary.
2. **Pool-agent endpoint** in `pool-agent/api/openapi/pool.yaml`, alongside the
   existing `/api/project/{projectId}/pool/{poolId}/sandboxes` routes.
3. **Server route**, conceptually
   `GET /projects/{projectId}/pools/{poolId}/utilization?includeSandboxes=true`.
   The server authorizes, acquires the existing pool-agent client lease, and
   proxies or normalizes. **It persists nothing** and emits no project events.
4. **Snapshot semantics.** The field names are yours to finalize in the OpenAPI
   design; these properties are the actual contract:
   - every response is timestamped, and carries the sample source;
   - cumulative counters are distinguishable from sampled rates and gauges (a
     monotonic CPU-nanoseconds counter is not the same thing as an averaged core
     count, and a consumer must be able to tell which it has, including the
     sample window);
   - pool and sandbox figures share a sample boundary as closely as practical;
   - unsupported metrics are **omitted or capability-flagged, never fabricated
     as zero**;
   - sandbox rows carry the stable Discobox sandbox ID for correlation;
   - termination and OOM information is included when the runtime can report it;
   - one sandbox failing to sample does not discard the whole pool snapshot;
   - an offline pool returns an explicit unavailable response, not stale or
     zeroed data.
5. One request returns the pool snapshot *including* its sandbox breakdown. A
   consumer UI must never need one request per sandbox.

## Out of scope

- Historical series, streaming, utilization events, billing-grade accounting,
  cross-pool aggregation. All explicitly deferred.
- Persisting samples anywhere, or copying them into `Pool`/`Sandbox` status.
- Feeding utilization back into placement or admission. ADR 0029 removed
  capacity-based admission; do not reintroduce it here.
- Replacing the existing heartbeat fields. They serve pool health; leave them.

## Design questions for the engineer

- **Proxy or normalize?** Passing the pool agent's response through is simpler;
  normalizing at the server gives one stable public shape across pool
  implementations (Docker, libkrun and vz microVMs, WSL, DigitalOcean, execvm;
  `docs/adr/0005-kubernetes-backend-is-a-worker-driver.md` is still `Proposed`
  and no Kubernetes provider exists). Given
  `docs/adr/0013-local-linux-pools-use-libkrun-microvms.md` and
  `docs/adr/0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md`,
  differing collection capability across backends is likely, which argues for normalizing plus an
  explicit capability signal.
- **Where do storage figures come from?** CPU and memory are cgroup reads;
  storage may need a volume or filesystem query with a different cost profile
  and sample cadence. It may not share the CPU/memory sample boundary.
- **Caching and cost.** Should the pool agent sample on a timer and serve the
  last sample, or collect on request? A timer bounds cost and makes the sample
  window well-defined; on-request is fresher. Note that upstream may apply its
  own short cache and will surface the original sample time regardless.
- **Reuse or retire the sandbox-agent per-exec collector?** It is per-exec and
  opaque, so it probably cannot serve this — but check before duplicating
  cgroup-parsing code across two modules.

## Done when

- One authenticated request returns a timestamped pool snapshot with a
  per-sandbox breakdown.
- A pool with one unsamplable sandbox still returns a usable snapshot that says
  which sandbox is missing.
- An offline pool returns an explicit unavailable response.
- Utilization reads provably create no project events and no persisted status
  churn.
- `pool-agent/DESIGN.md` and the server pool `DESIGN.md` describe the read path.
- `go tool task check-hooks` passes.
