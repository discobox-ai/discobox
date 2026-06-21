# Workers Design

`internal/resources/workers` owns worker API behavior, worker/provider-facing
management, and worker lifecycle reconciliation.

## Boundaries

```mermaid
flowchart LR
    api[internal/handlers] --> service[Service]
    providers[provider implementations] --> manager[Manager]
    dispatcher[orchestration.Dispatcher] --> executor[WorkerReconcileExecutor]
    executor --> reconciler[WorkerReconciler]
    manager --> store[internal/store]
    reconciler --> store
    reconciler --> runtime[sandbox worker runtime]
```

- `Service` handles worker registration, listing, and authenticated status
  updates.
- `Manager` is the narrow provider-facing interface for worker creation,
  bootstrap tokens, scheduling lookup, and cleanup decisions.
- `WorkerReconcileExecutor` decodes worker jobs and calls `WorkerReconciler`.
- Worker runtime cleanup/launch decisions must preserve generation checks.
