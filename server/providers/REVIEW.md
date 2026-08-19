# Provider Review Notes

- Backend-owned pool runtime drift detection should not mutate orchestrated
  resource lifecycle rows in place. Enqueue the pool host reconcile job for rows
  that still exist; direct provider cleanup is reserved for managed runtime
  orphans with no DB row.
- Keep pool-runtime drift detection separate from sandbox-runtime
  reconciliation. Even when both are Docker containers, drift detection may only
  observe pool runtimes; sandbox containers are owned by the pool-agent
  sandbox runtime and reached through sandbox reconciliation.
- Do not delete persisted pools as a repair strategy for failed reconciliation
  or pool downsizing unless the control plane has proven no sandbox is assigned
  to that pool. The engine may recreate the pool host container (and a driver its
  VM) for an existing pool, but must preserve the pool host row and pool ID.
- Pool repair must preserve state: container recreation keeps state in named
  volumes that survive container removal, and the engine replaces the VM only
  when it is missing or unhealthy. Never invoke repair from a pool host delete
  path.
- Terminal `failed` is only for pools that never completed create. Gate any
  new terminal-failure transition on `!Pool.EverCreated()`. A created pool
  that fails a reconcile/repair must go to a non-terminal state
  (`FailOperationRetryable`, e.g. `offline`) and keep being re-enqueued for
  reconciliation, not latched. Do not reintroduce checks that treat every
  `phase==failed`/`LastOperationStatus==failed` pool as terminal
  (`activePool`, the docker watcher, and pool repair all special-case
  `EverCreated`).
- Keep Docker out of `workerpool` and `internal/sandbox`. Container mechanics
  belong in `dockerworker.Engine`; anything backend-specific belongs behind
  `dockerworker.Driver`. A new backend should only implement VM CRUD plus the
  two connection methods.
- The pool host console must never carry `LabelPoolAgent` (drift detection
  reconciles and deletes what does) or the pool agent's
  `discobox.sandbox.managed` label. It is not a pool runtime and not a sandbox.
- Do not gate the console on pool readiness, registration, health, or a
  provider instance being enabled. It is opened when those are false; each
  check would withhold it exactly then. For the same reason it must keep
  reaching the host through the driver's Docker client, never through the pool
  agent.
- Keep console teardown in `RemovePool`. Nothing reconciles the console, so
  pool deletion is the only thing that removes it.
- Do not add optional provider metadata, status, or lifecycle interfaces.
  `sandbox.Provider`, `workerpool.WorkerProvider`, and `dockerworker.Driver`
  state required behavior directly. Optional feature interfaces need a runtime
  product reason, not a smaller diff.
- Drivers must not implement Docker readiness polling or carry bootstrap
  secrets in VM user data; the engine owns readiness waits and injects
  bootstrap identity as container environment.
