# Orchestration Review Notes

When adding or changing orchestrated resources:

- Add a model with `ResourceLifecycle` and event identity methods.
- Define desired states, phases, operations, and `OperationSpec` values.
- Add generation-aware store reads/writes.
- Ensure intent changes, resource events, and job rows are persisted in one transaction.
- Use a thin resource-specific wrapper around `orchestration.Begin`.
- Do not manually create job rows or resource events in API handlers.
- Reconcile one generation at a time; do not retarget running jobs to newer intent.
- Map generation conflicts to `jobqueue.Canceled(...)`.
