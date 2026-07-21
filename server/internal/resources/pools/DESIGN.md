# Pools Design

`internal/resources/pools` owns the `Pool` resource: the user-visible sharing
boundary sandboxes are scheduled into (ADR-0003), and its own runtime host
(ADR-0006). A pool binds to one provider instance at create time, immutably;
the provider instance is backend identity only, while capacity, cache, and the
runtime lifecycle live on the pool.

Pools are the one resource with **two front doors**, because they have two
very different kinds of caller:

```mermaid
flowchart LR
    handlers[HTTP handlers] --> svc["pools.Service<br/>(untrusted API surface)"]
    drivers["provider drivers<br/>(poolruntime, docker)"] -- sandbox.PoolManager --> cp["pools.ControlPlane<br/>(trusted control plane)"]
    svc --> store[(store)]
    svc -- SubmitPoolDelete / SchedulePoolReconciliation --> cp
    cp --> store
    cp --> engine[(reconcile engine)]
    engine --> rec[PoolReconciler]
    rec --> store
    rec -- runtime calls --> drivers
```

## Responsibilities

- `service.go` — pool CRUD plus intent submission. Create validates the
  backing provider instance and schedules the first reconcile; update never
  touches `ProviderInstanceID` (immutable) and re-schedules the reconcile so
  envelope changes converge. Delete requires the pool to be empty of
  sandboxes (assignment is immutable, so there is nothing to drain to),
  refuses built-in pools, and submits delete intent; the reconciler removes
  the runtime, then deletes the row.
- `agent_service.go` — the pool agent surface: bootstrap-token registration
  (`RegisterPool`), heartbeats (`UpdatePoolStatus`), and sandbox-removal
  reports, each verifying the authenticated **pool principal**.
- `controlplane.go` — trusted operations implementing `sandbox.PoolManager`:
  reads for drivers, bootstrap/agent token minting, the schedulable-pool
  placement gate, dirty marks (`SchedulePoolReconciliation`/`...At`), and
  repair intent (`SchedulePoolRepair`: generation bump + mark, so schedulers
  can tell a pending retry from a settled failure).
- `reconciler.go` — `PoolReconciler`: converges the pool's single runtime
  host (container/VM/pod) toward its desired state through the provider's
  `PoolRuntimeReconciler`. Active pools are drift-checked and repaired in
  place when sandboxes are assigned; a runtime whose agent never registers
  within the timeout is repaired with a fresh bootstrap token; delete removes
  the runtime and then the row. Failure latching follows `EverCreated`:
  never-registered pools may fail terminally, created pools drop to the
  retryable offline phase.

Reconciliation is level-triggered: intent writers mark `(pool, id)` dirty and
the engine (`internal/reconcile`) drives convergence; `ScanDirty` re-checks
every pool as the drift and lost-mark backstop.
