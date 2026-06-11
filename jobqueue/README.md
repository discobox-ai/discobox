# jobqueue

`jobqueue` is a reusable durable work queue for Go services.

It provides the queueing and dispatching primitive, but it does not define any
application job types. Application packages own their payload structs, executor
implementations, and service dependencies.

## Goals

- Durable jobs persisted through a `Store`.
- Typed application payloads without coupling the queue to domain packages.
- Resource-based deduplication and execution serialization across job types.
- Delayed scheduling.
- Priority-based claiming.
- Retry attempts with backoff.
- Per-job-type concurrency limits.
- Graceful dispatcher drain and shutdown.
- Optional multi-process leader election.
- Small public API that can be backed by GORM, memory stores for tests, or other
  persistence layers.

## Package Shape

The package has four main concepts:

- `Payload`: implemented by application-owned job payload structs.
- `Queue`: serializes payloads and persists durable `Job` rows.
- `Executor`: implemented by application code to process one job type.
- `Dispatcher`: claims runnable jobs and calls registered executors.

The dependency direction is:

```text
application payloads/executors -> jobqueue
jobqueue                     -> Store interface
Store implementation          -> database/runtime of choice
```

`jobqueue` should not import application packages.

## Payloads

Applications define payloads in their own packages:

```go
const TypeSandboxProvision jobqueue.Type = "sandbox.provision"

type SandboxProvisionPayload struct {
    ProjectID string `json:"projectId"`
    SandboxID string `json:"sandboxId"`
}

func (p SandboxProvisionPayload) JobType() jobqueue.Type {
    return TypeSandboxProvision
}

func (p SandboxProvisionPayload) Resource() jobqueue.Resource {
    return jobqueue.Resource{Type: "sandbox", ID: p.SandboxID}
}
```

`Resource` identifies the logical object being modified. By default, enqueue
coalesces pending jobs by resource. For example, a pending `sandbox.reconcile`
job for `sandbox-1` is updated to the latest payload instead of creating a
second pending row. A running job does not block enqueueing a newer pending job
for the same resource.

The dispatcher also serializes execution by resource, so two jobs for the same
resource should not run at the same time even when duplicates were allowed at
enqueue time.

Payloads may implement optional interfaces:

- `Prioritized`: overrides claim priority. Higher priority runs first.
- `MaxAttempter`: overrides maximum execution attempts.
- `Schedulable`: delays execution until a specific time.
- `DuplicateAllower`: permits this specific enqueue to create another active job
  for the same resource, even if the existing job has a different type. Execution
  is still serialized by resource.

## Queue

`Queue.Enqueue` accepts any `Payload`, applies defaults, serializes the payload
as JSON, and delegates storage to `Store.EnsureActiveJobForPayload`.

The default enqueue rule is resource-wide pending-job coalescing:

```text
one pending job per Resource, independent of job type
```

If a pending job already exists for the same resource, `Enqueue` returns that
job after the store updates it to the latest payload. If only a running job
exists, enqueue creates a newer pending job. Payloads can opt out per enqueue
by implementing `DuplicateAllower`.

```go
queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{
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

func (e *SandboxProvisionExecutor) Type() jobqueue.Type {
    return TypeSandboxProvision
}

func (e *SandboxProvisionExecutor) Execute(ctx context.Context, job *jobqueue.Job) error {
    var payload SandboxProvisionPayload
    if err := json.Unmarshal(job.Payload, &payload); err != nil {
        return err
    }
    return e.sandboxes.Provision(ctx, payload.ProjectID, payload.SandboxID)
}
```

The dispatcher passes a context bounded by `DispatcherConfig.JobTimeout`.
Returning an error fails the attempt and may schedule a retry if attempts remain.

## Dispatcher

The dispatcher is configured once and executors are registered at composition
time:

```go
dispatcher := jobqueue.NewDispatcher(store, jobqueue.DispatcherConfig{
    SingleNode:         true,
    PollInterval:       5 * time.Second,
    JobTimeout:         20 * time.Minute,
    StaleJobTimeout:    30 * time.Minute,
    RetryBackoff:       5 * time.Second,
    ImmediateExecution: true,
    DefaultConcurrency: 1,
})

err := dispatcher.Register(
    NewSandboxProvisionExecutor(sandboxService),
    jobqueue.WithConcurrency(2),
)
```

`WithConcurrency` limits how many jobs of that executor's type may run at the
same time on one dispatcher. Resource serialization still applies, so two jobs
for the same resource should not run concurrently even if concurrency is greater
than one.

## Job Lifecycle

Jobs move through these states:

```text
pending -> running -> completed
pending -> running -> pending    (retry)
pending -> running -> failed     (attempts exhausted)
running -> pending               (stale job recovery)
```

Claim order should be:

1. pending status
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
- `CreateJob(ctx, job, jobqueue.WithUniqueResource())` must atomically reject
  another active job for the same non-empty resource by returning
  `ErrJobAlreadyExists`.
- `EnsureActiveJobForPayload` is the canonical enqueue primitive. It should
  update and return an existing pending resource job, create a new pending job
  when only running jobs exist, and create a separate job row when the payload
  implements `DuplicateAllower`.
- Queue delegates enqueue behavior to `EnsureActiveJobForPayload`.
- Custom store implementations should call `jobqueue.ResolveCreateJobOptions`
  to interpret create options.
- `HasActiveJobForResource` checks pending and running jobs for a resource
  across all job types.
- `ClaimJob` marks exactly one eligible job running for the caller's worker ID.
- `ClaimJob` does not claim jobs whose resource already has a running job,
  regardless of type.
- `FailJob` either requeues with a retry delay or marks the job failed.
- `CancelJob` marks a job `canceled` without retrying it.
- `CleanupStaleJobs` recovers abandoned running jobs.
- Leadership methods are no-ops only for deployments using `SingleNode`.

## Store Implementations

`jobqueue` owns the queue and dispatcher abstractions, but not a database
persistence implementation. Applications provide an implementation of the
`jobqueue.Store` interface and pass the same store to both `NewQueue` and
`NewDispatcher`:

```go
queue := jobqueue.NewQueue(store, jobqueue.QueueConfig{
    DefaultPriority:    10,
    DefaultMaxAttempts: 3,
})

dispatcher := jobqueue.NewDispatcher(store, jobqueue.DispatcherConfig{
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

## Graceful Shutdown

`BeginDrain` prevents the dispatcher from claiming new jobs while allowing
in-flight work to finish.

`DrainAndStop(ctx)` enters drain mode, waits for running jobs, stops background
loops, and releases leadership when applicable. If the context expires, durable
stale-job cleanup is responsible for later recovery.

## Example

See `example/` for a compiling example that defines sandbox payloads, executors,
composition wiring, enqueue helpers, a GORM SQLite store, and a runnable main:

```sh
go run ./example
```
