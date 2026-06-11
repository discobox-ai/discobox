# Orchestration Design

This standalone module provides durable orchestration primitives without depending
on application resources, stores, or lifecycle models.

## Package Shape

The module has two layers:

- Durable job queue primitives: `Payload`, `Job`, `Queue`, `Store`, `Executor`,
  and `Dispatcher`.
- Reconciled-resource submission: `Submitter`, which atomically records accepted
  resource intent and appends the durable job that will reconcile it.

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

## Reconciled Resource Submission

`Submitter` captures the desired-state orchestration pattern:

1. Run inside an application-provided transaction.
2. Prepare the resource by either using a new resource (`Create`) or loading an
   existing one (`Submit`).
3. Increment resource generation.
4. Apply the accepted operation.
5. Build the application payload for the updated resource.
6. Append the durable job in the same transaction.
7. Set the resource's last job ID.
8. Persist the resource with the application-provided create or update function.
9. After commit, notify the dispatcher as a wakeup optimization when a job row
   was created.

The transaction, resource store, operation type, payload shape, and notification
mechanism are all supplied by the application.

## Append-Only Jobs

Every accepted unit of work appends a new durable job row. Queue and submitter
logic must not rewrite an existing job's payload, type, schedule, or resource to
represent newer intent.

Reconciled resources should put their accepted generation in the payload. An
executor can implement `GenerationAssertor` to let the dispatcher assert that
the resource still matches the payload generation before calling `Execute`.
Generation conflicts should return `Superseded(...)` so the dispatcher cancels
the job instead of failing it.

Observed-generation updates are resource-specific and remain the reconciler's
responsibility. Some reconcilers may complete a job without treating the payload
generation as fully observed.

Dispatch remains resource-serialized: multiple pending jobs may exist for the
same resource, but the dispatcher must not run two jobs for that resource at the
same time.

## Dependency Direction

```text
application services/resources/stores -> orchestration
orchestration                         -> interfaces only
```

The module must not import application packages.
