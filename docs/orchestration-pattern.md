# Orchestration Pattern

This project uses a desired-state reconciliation pattern. API handlers record
intent on resource rows, persist a project event, and ensure a durable reconcile
job. Jobs execute straight-line reconciliation for one resource generation. If
new intent arrives, the old job cancels and a newer pending job handles the new
generation.

## Resource Shape

Orchestrated resources should embed `model.ResourceLifecycle`.

```go
type Example struct {
    ID        string `json:"id" gorm:"primaryKey;type:text"`
    ProjectID string `json:"projectId" gorm:"type:text;index"`

    model.ResourceLifecycle `gorm:"embedded"`
}
```

The lifecycle provides:

- `desiredState`: requested steady state.
- `phase`: observed user-facing phase.
- `activeOperation`: latest user-visible operation reason.
- `lastOperationStatus`: `pending`, `running`, `success`, or `failed`.
- `lastJobId`: latest reconcile job row.
- `generation`: incremented for every accepted intent.
- `observedGeneration`: latest generation fully reconciled.
- `statusMessage` / `errorMessage`: progress and failure detail.

The resource must also implement project event identity:

```go
func (r *Example) EventProjectID() string { return r.ProjectID }
func (r *Example) EventResourceType() string { return "example" }
func (r *Example) EventResourceID() string { return r.ID }
```

## Operation Specs

Each resource defines operation specs. Specs describe user intent, not rigid FSM
rules.

```go
var ExampleStartOperation = model.OperationSpec{
    Operation:    "start",
    DesiredState: "running",
    Phase:        "starting",
}
```

API actions should generally accept intent from any non-terminal state. Avoid
framework-level pre-state transition tables; let reconciliation decide what work
is needed from current observed/provider state.

## Store Methods

Keep one `store.Store` type, split methods into files by resource. Each
orchestrated resource usually needs:

- `List<Resource>s`
- `Get<Resource>(ctx, projectID, id, ...options)`
- `Create<Resource>`
- `Update<Resource>(ctx, resource, ...options)`
- snapshot/list methods if the resource participates in list-watch

Generation-aware reads and writes should return `store.ErrGenerationConflict`
when the expected generation no longer matches.

```go
resource, err := s.GetExample(ctx, projectID, id, store.WithGeneration(generation))

err := s.UpdateExample(ctx, resource, store.WithGeneration(generation))
```

Create/update methods should use the shared resource-event helper so every
persisted resource change records a project event with the full resource
snapshot.

## Job Payload

Each resource has one reconcile job type. The payload must include enough IDs to
reload the resource and the generation the job is responsible for.

```go
const ExampleReconcileType jobqueue.Type = "example.reconcile"

type ExampleReconcilePayload struct {
    ProjectID  string `json:"projectId"`
    ExampleID  string `json:"exampleId"`
    Generation int64  `json:"generation"`
}
```

`jobqueue` treats payloads as opaque JSON. Generation is a resource/controller
concept, not a jobqueue concept.

## Thin Orchestrator

Create a resource-specific thin wrapper around `orchestration.Begin`. This keeps
service methods readable and centralizes the generic callback boilerplate.

```go
type ExampleOrchestrator struct {
    store        *store.Store
    orchestrator *orchestration.Orchestrator
}

func (o *ExampleOrchestrator) Create(ctx context.Context, example *model.Example) (*model.Example, error) {
    return orchestration.Begin(ctx, o.orchestrator, model.ExampleCreateOperation,
        func(context.Context, *store.Store) (*model.Example, error) {
            return example, nil
        },
        func(ctx context.Context, txStore *store.Store, resource *model.Example) error {
            return txStore.CreateExample(ctx, resource)
        },
        exampleReconcilePayload,
    )
}
```

The payload builder runs after `orchestration.Begin` increments generation, so
the job payload carries the accepted generation.

## API Service

API service methods should:

1. Validate parent resources and request shape.
2. Build or load the model.
3. Delegate intent changes to the resource-specific orchestrator.

They should not manually create job rows or project events.

## Reconciler

Reconcile jobs should be straight-line for one generation.

1. Load the resource with `WithGeneration(payload.Generation)`.
2. If generation does not match, return `jobqueue.Canceled("... generation changed")`.
3. Perform provider work for the desired state.
4. Write progress/completion with `Update<Resource>(..., WithGeneration(...))`.
5. On success set `observedGeneration = payload.Generation`.
6. On generation conflict, return `jobqueue.Canceled(...)`.

Do not retarget an in-flight job to newer intent. New intent creates or updates
a newer pending job, and the current job cancels at the next generation check.

Keep reconciliation coordination separate from provider mechanics:

- `<Resource>Reconciler`: loads generation-scoped resources, writes lifecycle
  progress, maps generation conflicts to `jobqueue.Canceled`, and chooses which
  operation to run.
- `<Resource>Operations`: performs the actual provider/domain work such as
  start, stop, delete, provision, or cleanup. It should not know about API DTOs,
  project events, or jobqueue.

## Jobqueue Semantics

The canonical enqueue primitive is `Store.EnsureActiveJobForPayload`.

- Pending jobs for the same resource are coalesced and updated to the latest
  payload.
- A running job does not block creating a newer pending job.
- The dispatcher still serializes execution by resource.
- `jobqueue.Canceled(...)` records terminal `canceled` status without retry.

Dispatcher notification is only a wakeup optimization after commit. Durable rows
are the source of truth.

## New Resource Checklist

- Add model with `ResourceLifecycle` and event identity methods.
- Define desired states, phases, operations, and `OperationSpec` values.
- Add store file with CRUD, generation-aware get/update, and snapshots if needed.
- Add reconcile payload and executor in `internal/jobs`.
- Add thin resource orchestrator wrapper.
- Add resource operations for provider/domain mechanics.
- Add resource reconciler for generation-scoped job coordination.
- Wire service methods to the wrapper.
- Register executor with the dispatcher.
- Add Huma operations and API tests.
- Add reconcile tests for generation cancellation and successful observation.
