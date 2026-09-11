# Service Review Notes

- API service methods accept intent; they never perform provider work. Intent
  is the generation bump and desired-state write, committed in the same
  transaction as the reconcile engine's `MarkDirtyTx`, so accepted intent can
  never lose the mark that drives it.
- Avoid strict lifecycle pre-state tables; the reconciler decides work from the
  latest persisted desired and observed state.
- Reconciler writes are guarded by the generation the reconcile loaded
  (`store.WithGeneration`, `UpdatePoolWithGeneration`). A lost guard
  (`store.ErrGenerationConflict`) returns `reconcile.Superseded`, and the
  reconciler settles cleanly on `reconcile.ErrSuperseded`: the newer intent's
  own mark re-runs it against current state. Never retry stale intent.
- Start, stop, and restart are instructions forwarded to the pool agent, not
  desired state: they bump no generation and add no power desired state
  (ADR 0017 §9, ADR 0034).
- A reconciler never marks the resource it is reconciling (`ErrSelfMark`); to
  run again later it returns `Result.RequeueAt`/`RequeueAfter`.
- Provider operations return runtime state for the reconciler to persist; they
  do not write resource rows. Nothing publishes change events — there is no
  event broker (ADR 0081) — so a waiter polls the rows.
