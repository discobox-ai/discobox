# Orchestration Review Notes

- Keep this module application-neutral. Do not import `internal/...` packages,
  concrete resource models, API DTOs, or application stores.
- Preserve the atomicity contract for `Submitter`: resource intent and durable
  reconcile job creation must happen in the same application transaction.
- `Submitter` should know only the generic lifecycle methods on the resource and
  caller-provided functions for transactions, persistence, payload construction,
  job ensuring, and notification.
- Dispatcher notification is only a wakeup optimization. Durable job rows remain
  the source of truth.
- Keep jobs append-only. Queue and submitter paths must create new job rows
  instead of rewriting existing payloads to represent newer intent.
- Keep submission backoff application-neutral and keyed by job type, resource
  type, and resource ID. Do not collapse different job types or resource types
  into the same backoff bucket.
- Keep job result data separate from failure state. Use `JobResult.Message` and
  `JobResult.Metadata` for operator/result data; keep `Job.Error` for execution
  or dispatch failures.
- Keep job execution resource-serialized. Multiple pending jobs may exist for a
  resource, but the dispatcher must not run two jobs for the same resource
  concurrently.
- Do not add resource-specific lifecycle rules here. Domain decisions belong in
  application services and reconcilers.
- Store implementations must uphold method-level atomicity, especially job
  claiming and active resource ownership.
