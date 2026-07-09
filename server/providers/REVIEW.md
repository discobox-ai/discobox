# Provider Review Notes

- Backend-owned worker runtime drift detection should not mutate orchestrated
  resource lifecycle rows in place. Enqueue the worker reconcile job for rows
  that still exist; direct provider cleanup is reserved for managed runtime
  orphans with no DB row.
- Keep worker-runtime drift detection separate from sandbox-runtime
  reconciliation. Even when both are Docker containers, drift detection may only
  observe worker runtimes; sandbox containers are owned by the worker-agent
  sandbox runtime and reached through sandbox reconciliation.
- Do not delete persisted workers as a repair strategy for failed reconciliation
  or pool downsizing unless the control plane has proven no sandbox is assigned
  to that worker. The engine may recreate the worker container (and a driver its
  VM) for an existing worker, but must preserve the worker row and worker ID.
- Worker repair must preserve state: container recreation keeps state in named
  volumes that survive container removal, and the engine replaces the VM only
  when it is missing or unhealthy. Never invoke repair from a worker delete
  path.
- Keep Docker out of `workerpool` and `internal/sandbox`. Container mechanics
  belong in `dockerworker.Engine`; anything backend-specific belongs behind
  `dockerworker.Driver`. A new backend should only implement VM CRUD plus the
  two connection methods.
- Do not add optional provider metadata, status, or lifecycle interfaces.
  `sandbox.Provider`, `workerpool.WorkerProvider`, and `dockerworker.Driver`
  state required behavior directly. Optional feature interfaces need a runtime
  product reason, not a smaller diff.
- Drivers must not implement Docker readiness polling or carry bootstrap
  secrets in VM user data; the engine owns readiness waits and injects
  bootstrap identity as container environment.
