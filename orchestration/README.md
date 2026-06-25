# orchestration

`orchestration` is a reusable durable work queue for Go services.

It provides the queueing and dispatching primitive, but it does not define any
application job types. Application packages own their payload structs, executor
implementations, and service dependencies.

## Goals

- Durable jobs persisted through a `Store`.
- Typed application payloads without coupling the queue to domain packages.
- Append-only job creation with resource-based execution serialization.
- Delayed scheduling.
- Priority-based claiming.
- Retry attempts with fixed delayed rescheduling.
- Submission backoff for repeated jobs on the same job type/resource type/resource ID.
- Per-job-type concurrency limits.
- Graceful dispatcher drain and shutdown.
- Optional multi-process leader election.
- Small public API that can be backed by GORM, memory stores for tests, or other
  persistence layers.

## Package Shape

The package has five main concepts:

- `Payload`: implemented by application-owned job payload structs.
- `Queue`: serializes payloads and persists durable `Job` rows.
- `Executor`: implemented by application code to process one job type.
- `Dispatcher`: claims runnable jobs and calls registered executors.
- `Dispatcher.Submit`: appends durable jobs directly or through an
  application-owned transaction wrapper.

The dependency direction is:

```text
application payloads/executors -> orchestration
orchestration                -> Store interface
Store implementation          -> database/runtime of choice
```

`orchestration` should not import application packages.

## Payloads

Applications define payloads in their own packages:

```go
const TypeSandboxProvision orchestration.Type = "sandbox.provision"

type SandboxProvisionPayload struct {
    ProjectID string `json:"projectId"`
    SandboxID string `json:"sandboxId"`
}

func (p SandboxProvisionPayload) JobType() orchestration.Type {
    return TypeSandboxProvision
}

func (p SandboxProvisionPayload) Resource() orchestration.Resource {
    return orchestration.Resource{Type: "sandbox", ID: p.SandboxID}
}
```

`Resource` identifies the logical object being modified. Enqueue is append-only:
each accepted payload creates a new durable job row. Existing job rows are not
rewritten to represent newer intent.

The dispatcher serializes execution by resource, so two jobs for the same
resource should not run at the same time even when multiple pending jobs exist.

Payloads may implement optional interfaces:

- `Prioritized`: overrides claim priority. Higher priority runs first.
- `MaxAttempter`: overrides maximum execution attempts.
- `Schedulable`: delays execution until a specific time.

## Executors

Executors handle one application-owned job type:

```go
type Executor interface {
    Execute(context.Context, *orchestration.Job) (orchestration.JobResult, error)
}
```

`JobResult.Message` is a human-readable result note for operators.
`JobResult.Metadata` is structured JSON result data. The dispatcher persists the
result when a job completes, fails, or is canceled. Returning an error still
controls retry/failure behavior; the job's `error` field remains separate from
result message and metadata.

## Queue

`Queue.Enqueue` accepts any `Payload`, applies defaults, serializes the payload
as JSON, and appends a new durable job row with `Store.CreateJob`.

```go
queue := orchestration.NewQueue(store, orchestration.QueueConfig{
    DefaultPriority:    10,
    DefaultMaxAttempts: 3,
})

job, err := queue.Enqueue(ctx, SandboxProvisionPayload{
    ProjectID: projectID,
    SandboxID: sandboxID,
})
```

The queue can notify a dispatcher after persistence succeeds:

```go
queue.SetNotifyFunc(dispatcher.NotifyNewJob)
```

The notification path is an optimization. Polling should still discover jobs if
notifications are disabled or missed.

## Executors

Executors live in application packages because they depend on concrete payloads
and domain services.

```go
type SandboxProvisionExecutor struct {
    sandboxes SandboxService
}

func (e *SandboxProvisionExecutor) Execute(ctx context.Context, job *orchestration.Job) (orchestration.JobResult, error) {
    var payload SandboxProvisionPayload
    if err := json.Unmarshal(job.Payload, &payload); err != nil {
        return orchestration.JobResult{}, err
    }
    return orchestration.JobResult{}, e.sandboxes.Provision(ctx, payload.ProjectID, payload.SandboxID)
}
```

The dispatcher passes a context bounded by `DispatcherConfig.JobTimeout`.
Returning an error fails the attempt and may schedule a retry if attempts remain.

## Dispatcher

The dispatcher is configured once and executors are registered at composition
time:

```go
dispatcher := orchestration.NewDispatcher(store, orchestration.DispatcherConfig{
    SingleNode:         true,
    PollInterval:       5 * time.Second,
    JobTimeout:         20 * time.Minute,
    StaleJobTimeout:    30 * time.Minute,
    RetryBackoff:       5 * time.Second,
    ImmediateExecution: true,
    DefaultConcurrency: 1,
})

err := dispatcher.Register(
    TypeSandboxProvision,
    NewSandboxProvisionExecutor(sandboxService),
    orchestration.WithConcurrency(2),
)
```

`WithConcurrency` limits how many jobs of that registered type may run at the
same time on one dispatcher. Resource serialization still applies, so two jobs
for the same resource should not run concurrently even if concurrency is greater
than one.

## Job Lifecycle

Jobs move through these states:

```text
pending -> running -> completed
pending -> running -> pending    (retry)
pending -> backoff               (submission backoff)
backoff -> running               (scheduled time reached and claimed)
pending -> running -> failed     (attempts exhausted)
pending -> canceled              (superseded or explicitly canceled)
running -> canceled              (executor reports cooperative cancellation)
running -> pending               (stale job recovery)
```

Claim order should be:

1. pending or backoff status
2. scheduled time has arrived
3. registered type has dispatcher capacity
4. resource has no other running job
5. highest priority first
6. oldest scheduled time first
7. oldest creation time as a tie-breaker

## Store Contract

`Store` is the persistence boundary for both queue and dispatcher. A production
store must make `ClaimJob` atomic across concurrent dispatchers.

Important expectations:

- `CreateJob` assigns IDs and timestamps when absent.
- `CreateJob` atomically cancels any older pending/backoff job with the same job
  type and resource before appending the latest queued row.
- `CreateJob(ctx, job, orchestration.WithUniqueResource())` must atomically reject
  another active job for the same non-empty resource by returning
  `ErrJobAlreadyExists`.
- Normal queue and dispatcher submit paths append jobs through `CreateJob` and
  must not rewrite existing job payloads.
- Custom store implementations should call `orchestration.ResolveCreateJobOptions`
  to interpret create options.
- `HasActiveJobForResource` checks pending, backoff, and running jobs for a resource
  across all job types.
- `ClaimJob` marks exactly one eligible job running for the caller's worker ID.
- `ClaimJob` does not claim jobs whose resource already has a running job,
  regardless of type.
- `FailJob` either requeues with a retry delay, cancels itself when a queued
  successor for the same type/resource already exists, or marks the job failed.
- `CancelJob` marks a job `canceled` without retrying it.
- `CleanupStaleJobs` recovers abandoned running jobs.
- Leadership methods are no-ops only for deployments using `SingleNode`.

## Store Implementations

`orchestration` owns the queue and dispatcher abstractions, but not a database
persistence implementation. Applications provide an implementation of the
`orchestration.Store` interface and pass the same store to both `NewQueue` and
`NewDispatcher`:

```go
queue := orchestration.NewQueue(store, orchestration.QueueConfig{
    DefaultPriority:    10,
    DefaultMaxAttempts: 3,
})

dispatcher := orchestration.NewDispatcher(store, orchestration.DispatcherConfig{
    SingleNode:         true,
    PollInterval:       5 * time.Second,
    JobTimeout:         20 * time.Minute,
    StaleJobTimeout:    30 * time.Minute,
    RetryBackoff:       5 * time.Second,
    ImmediateExecution: true,
    DefaultConcurrency: 1,
})
```

For multi-process deployments, the store implementation must provide durable
job state and leadership methods. The server application in this repository
implements persistence in its own storage layer.

## Leader Election

For one-process deployments, set `SingleNode: true`.

For multi-process deployments, stores use `TryAcquireLeadership` and
`ReleaseLeadership` to ensure only one dispatcher claims jobs at a time. The
leader periodically renews ownership using `HeartbeatInterval`; another process
may take over after `HeartbeatTimeout`.

## Dispatcher Submit

`Dispatcher.Submit` appends a durable job and wakes the dispatcher after the job
is created. Application code that needs resource intent and job append to commit
together should pass `WithSubmitTransaction`. The transaction wrapper receives an
append callback, can build the payload after mutating resource intent, and gets
the created `Job` back so it can store the last job ID before committing.

Generation-scoped executors can implement `GenerationAssertor`. The dispatcher
invokes it after claiming a job and before calling `Execute`, allowing stale
generation jobs to return `Superseded(...)` and be stored as canceled without
running domain logic. This pre-execute assertion intentionally does not update
observed generation; that remains a reconciler-owned resource state transition.

Resource versioning, last-job bookkeeping, and persistence are application-owned;
orchestration only knows the transaction-scoped value satisfies `JobStore`.

## Graceful Shutdown

`BeginDrain` prevents the dispatcher from claiming new jobs while allowing
in-flight work to finish.

`DrainAndStop(ctx)` enters drain mode, waits for running jobs, stops background
loops, and releases leadership when applicable. If the context expires, durable
stale-job cleanup is responsible for later recovery.

## Example

See `example/` for a compiling sandbox-style example that wires a
small app-owned sandbox manager, in-memory resource/job persistence, a
`sandbox.reconcile` payload, a dispatcher executor, and a runnable main:

```sh
go run ./example
```
