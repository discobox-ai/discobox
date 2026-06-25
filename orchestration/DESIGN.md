# Orchestration Design

This standalone module provides durable orchestration primitives without depending
on application resources, stores, or lifecycle models.

## Package Shape

The module has two layers:

- Durable job queue primitives: `Payload`, `Job`, `Queue`, `Store`, `Executor`,
  and `Dispatcher`.
- Submission helpers: `Dispatcher.Submit` appends durable jobs directly or
  through an application-supplied transaction wrapper.

Application packages own concrete payloads, executors, resources, operations,
and store implementations. This module depends only on interfaces and generic
function types supplied by the application.

## Store Boundary

`Store` is the low-level durable job persistence boundary for `Queue` and
`Dispatcher`. Implementations are expected to make job creation, claiming,
completion, failure, cancellation, stale cleanup, and leadership atomic where
required by each method contract.

The module intentionally does not provide a database implementation. Applications
can back the interfaces with GORM, memory stores, or other persistence layers.

## Submission

`Dispatcher.Submit` owns the common non-generic durable append sequence: append
a payload with `QueueConfig` and notify the dispatcher after a job is created.
Callers that need resource mutation and job append to share a commit can pass an
optional transaction wrapper. That wrapper receives an append callback, can pass
the transaction-built payload to it, and gets the created `Job` back, so
application code can increment versions, set a last job ID, persist resources,
or do nothing at all without orchestration knowing those resource semantics.

## Append-Only Jobs

Every accepted unit of work appends a new durable job row. Queue and dispatcher
submission logic must not rewrite an existing job's payload, type, schedule, or
resource to represent newer intent.

Submission is coalesced while work is already queued for the same complete work
identity:

```text
job type + resource type + resource id
```

At most one `pending`/`backoff` job and one `running` job may exist for that
identity. A running job may have one queued successor. A newer queued submission
should cancel the older queued row before appending the latest payload; a running
job that fails while a queued successor already exists should be canceled instead
of requeued.

Reconciled resources should put their accepted generation in the payload. An
executor can implement `GenerationAssertor` to let the dispatcher assert that
the resource still matches the payload generation before calling `Execute`.
Generation conflicts should return `Superseded(...)` so the dispatcher cancels
the job instead of failing it.

Observed-generation updates are resource-specific and remain the reconciler's
responsibility. Some reconcilers may complete a job without treating the payload
generation as fully observed.

Executors return `JobResult` plus `error`. The dispatcher persists result
message and metadata on completed, failed, and canceled jobs. The `error` field
remains the failure/debug channel and is not overloaded with successful result
data.

Dispatch remains resource-serialized: multiple pending jobs may exist for the
same resource across different job types, but the dispatcher must not run two
jobs for that resource at the same time.

## Resource Backoff

Queue append applies core submission backoff before creating a job row. Backoff
is keyed by the complete resource work identity:

```text
job type + resource type + resource id
```

When recent jobs for that key exceed the configured threshold, the newly
appended job is stored with status `backoff` and a future `scheduled_at`.
Stores still treat `backoff` as active resource ownership and dispatchers may
claim it only after `scheduled_at` is reached. Retry delay for a failed attempt
is different: the same job row remains `pending` with a fixed short future
`scheduled_at`.

Default submission backoff starts after ten recent jobs for the same key within
fifteen minutes. The first delayed job waits thirty seconds, then doubles up to
a fifteen minute cap.

Applications provide the recent-job count through `Store`; orchestration owns
the threshold, window, delay calculation, and status transition. Domain-specific
decisions about whether to enqueue more work after terminal job events remain in
application services or reconcilers.

## Dependency Direction

```text
application services/resources/stores -> orchestration
orchestration                         -> interfaces only
```

The module must not import application packages.
