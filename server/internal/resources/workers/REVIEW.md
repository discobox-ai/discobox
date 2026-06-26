# Workers Review Notes

- Treat workers as stateful placement records. Do not add worker delete paths
  that bypass `CountSandboxesForWorker` or equivalent assignment checks.
- Failed worker reconcile jobs should mark workers failed or unschedulable, not
  deleted. Replacement capacity belongs to worker-provider reconciliation.
- Runtime replacement for an existing worker must preserve the worker row and
  worker ID. Use active worker reconciliation for driver-level recreate behavior.
- Do not call worker repair from delete reconciliation. Repair is only for active
  workers whose assigned sandboxes make deletion unsafe.
- Keep worker delete terminal state strict: `phase=deleted` should mean runtime
  cleanup succeeded, `revoked_at` is set, and no sandbox remains assigned.
