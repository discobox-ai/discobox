# Service Review Notes

- API service methods should accept intent, not perform provider work directly.
- Avoid strict lifecycle pre-state tables; reconciliation decides work from observed state.
- Keep reconcile executor coordination separate from provider operations.
- Reconciler writes must use generation-aware updates.
- On generation conflict, cancel the job instead of retrying stale intent.
- Restart should use `restartGeneration` / `restartedGeneration`, not a separate desired state.
- Provider operations should update runtime/secret state on the model but not emit events directly.
