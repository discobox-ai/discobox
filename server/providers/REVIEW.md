# Provider Review Notes

- Driver-owned worker runtime drift detection should not mutate orchestrated
  resource lifecycle rows in place. Enqueue the worker reconcile job for rows
  that still exist; direct provider cleanup is reserved for managed runtime
  orphans with no DB row.
- Keep worker-runtime drift detection separate from sandbox-runtime
  reconciliation. Even when both are Docker containers, drift detection may only
  observe worker runtimes; sandbox containers are owned by the worker-agent
  sandbox runtime and reached through sandbox reconciliation.
- Do not delete persisted workers as a repair strategy for failed reconciliation
  or pool downsizing unless the control plane has proven no sandbox is assigned
  to that worker. Runtime drivers may recreate the underlying VM/container for
  an existing worker, but must preserve the worker row and worker ID.
- Worker repair hooks must preserve state. For Docker this means container
  recreation must keep state in named volumes that survive container removal.
  Never invoke repair from a worker delete path.
- Do not make the generic VM worker-pool provider assume repair means deleting a
  VM. `vm.Driver.RepairWorkerVM` is required; pass the worker ID, current
  instance ID, desired `InstanceSpec`, and reason to the driver and let it
  choose the platform-specific repair action.
