# Resources Design

`internal/resources` contains resource-owned behavior. Each child package owns
the service, manager, executor, payload, and reconciliation code that applies to
one resource area.

## Boundaries

```mermaid
flowchart LR
    handlers[internal/handlers] --> contracts[internal/services]
    contracts --> service[internal/service]
    service --> resources[internal/resources/{resource}]
    dispatcher[orchestration.Dispatcher] --> executor[resource Executor]
    executor --> manager[resource Manager/Reconciler]
    resources --> store[internal/store]
```

- Resource packages may call stores for simple CRUD and use managers for
  lifecycle side effects.
- Executors must stay thin and delegate resource policy to managers or
  reconcilers.
- Keep HTTP transport adaptation in `internal/handlers`, service contracts in
  `internal/services`, and persistence in `internal/store`.
