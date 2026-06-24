# Provider Review Notes

- Provider inventory should not mutate orchestrated resource lifecycle rows in
  place. Enqueue the resource reconcile job for rows that still exist; direct
  provider cleanup is reserved for managed runtime orphans with no DB row.
- Keep worker-runtime inventory separate from sandbox-runtime reconciliation.
  Even when both are Docker containers, provider inventory may only observe
  worker runtimes; sandbox containers are owned by the worker-agent sandbox
  runtime and reached through sandbox reconciliation.
