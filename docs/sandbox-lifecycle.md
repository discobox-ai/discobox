# Sandbox Lifecycle

Sandbox state is modeled as a desired-state reconciliation resource, similar to
a list-watch API. The API resource records the user's desired steady state, the
currently observed phase, and the latest user intent that requested
reconciliation.

The reusable implementation is `model.ResourceLifecycle`. Resources embed that
struct so `desiredState`, `phase`, `activeOperation`, `lastOperationStatus`,
`lastJobId`, `generation`, `observedGeneration`, `statusMessage`, and
`errorMessage` remain flat DB/API fields while sharing the same operation
transition helpers. Each resource then defines resource-specific
`model.OperationSpec` values such as `SandboxStartOperation` or
`SandboxStopOperation`. Those specs describe user intent; they do not define a
rigid finite-state-machine transition.

## Fields

| Field | Meaning |
| --- | --- |
| `desiredState` | User intent. One of `running`, `stopped`, or `deleted`. |
| `phase` | Observed lifecycle phase. This is what clients usually display. |
| `activeOperation` | The operation currently queued or running, if any. |
| `lastOperationStatus` | State of the latest lifecycle operation/job. |
| `lastJobId` | The durable job ID for the latest lifecycle operation, once jobs are wired. |
| `generation` | Monotonic desired-state generation incremented for each accepted intent. |
| `observedGeneration` | Latest generation fully handled by reconciliation. |
| `statusMessage` | Human-readable progress detail. |
| `errorMessage` | Human-readable failure detail. |
| `restartGeneration` | Monotonic user intent counter for restarts. |
| `restartedGeneration` | Latest restart generation completed by reconciliation. |

## Desired State

`desiredState` is stable intent, not a transient progress marker.

| Value | Meaning |
| --- | --- |
| `running` | The sandbox should eventually be running. |
| `stopped` | The sandbox should eventually be stopped but retained. |
| `deleted` | The sandbox should eventually be removed. |

## Phase

`phase` describes observed progress toward the desired state.

| Value | Meaning |
| --- | --- |
| `pending` | Resource exists but create/provision work has not completed. |
| `provisioning` | Provider resources are being created. |
| `starting` | Runtime is starting. |
| `running` | Runtime is running and usable. |
| `stopping` | Runtime is stopping. |
| `stopped` | Runtime is retained but inactive. |
| `deleting` | Runtime/provider resources are being deleted. |
| `deleted` | Deletion completed. |
| `failed` | Latest operation failed and needs retry or operator action. |

## Operations

CRUD/action endpoints update resource intent and enqueue a single reconcile job
for the resource. The job is responsible for deciding what provider operation is
needed from the current observed state.

| API action | `desiredState` | `phase` | `activeOperation` | `lastOperationStatus` |
| --- | --- | --- | --- | --- |
| create | `running` | `pending` | `create` | `pending` |
| start | `running` | `starting` | `start` | `pending` |
| stop | `stopped` | `stopping` | `stop` | `pending` |
| restart | `running`, with `restartGeneration` incremented | `starting` | `restart` | `pending` |
| delete | `deleted` | `deleting` | `delete` | `pending` |

The durable job type is `sandbox.reconcile`. It updates `phase`,
`lastOperationStatus`, `lastJobId`, `statusMessage`, `errorMessage`, and
provider runtime fields as work progresses. Every persisted resource change
emits a project resource event.

## Reusable Job Intent Pattern

Every orchestrated resource should follow the same write order:

1. Load the resource.
2. Increment `generation` and update desired state/user-visible operation reason.
3. Ensure a durable reconcile job row carrying the new generation.
4. Set `lastJobId` to the ensured job row.
5. Persist the resource change and resource event in the same transaction.
6. Commit, then notify the dispatcher as a wakeup optimization.

The API handler owns intent acceptance. It should avoid strict pre-state
validation beyond obvious terminal or malformed requests. The job runner owns
observed-state interpretation after that point.

The orchestrator is required for DB-backed services. It receives an injected
job-row writer, not a queue. The application decides which database-backed job
store implementation writes the row, while the orchestrator coordinates one
GORM transaction across the resource store, resource event, and job row.

When a job starts, it reloads the resource and compares the payload generation
to the current resource `generation`. If they differ, the job returns
`jobqueue.Canceled` because it has been superseded. During reconciliation,
progress and completion writes use optimistic generation updates. If a write
affects zero rows, the store returns a generation conflict and the job is
canceled.

## API Action Behavior

API actions generally accept intent from any non-terminal state and ensure or
reuse a reconcile job. The framework should not enforce resource-specific FSM
transition tables.

| Action | Allowed current phases | Resulting intent |
| --- | --- | --- |
| create | none; resource does not exist | `desiredState=running`, `phase=pending`, `activeOperation=create` |
| start | any non-deleted resource | `desiredState=running`, `phase=starting`, `activeOperation=start` |
| stop | any non-deleted resource | `desiredState=stopped`, `phase=stopping`, `activeOperation=stop` |
| restart | any non-deleted resource | `desiredState=running`, `phase=starting`, `activeOperation=restart` |
| delete | any phase except `deleted` | `desiredState=deleted`, `phase=deleting`, `activeOperation=delete` |

Idempotent cases:

| Action | Current state | Behavior |
| --- | --- | --- |
| start | `desiredState=running` and phase is `pending`, `provisioning`, `starting`, or `running` | Return current resource; do not enqueue a duplicate start. |
| stop | `desiredState=stopped` and phase is `stopping` or `stopped` | Return current resource; do not enqueue a duplicate stop. |
| delete | `desiredState=deleted` and phase is `deleting` or `deleted` | Return current resource or 404 after hard deletion policy is chosen. |

Conflicting user actions are resolved by generation. Pending reconcile jobs for
the same resource are coalesced to the latest payload/generation. A running job
does not block creating a newer pending job; the running job cancels at its next
generation checkpoint, then the newer pending job runs.

Restart is not a steady desired state. It is represented by incrementing
`restartGeneration` while keeping `desiredState=running`. The reconciler treats
`restartGeneration > restartedGeneration` as work to perform, then copies
`restartGeneration` to `restartedGeneration` when the restart has completed.

## Phase Transitions

The job runner owns observed phase transitions. API handlers should set the
initial operation phase, then jobs should move through the allowed paths below.

### Create

```text
pending -> provisioning -> starting -> running
pending -> failed
provisioning -> failed
starting -> failed
```

Successful completion:

```text
phase=running
activeOperation=null
lastOperationStatus=success
errorMessage=null
```

### Start

```text
stopped -> starting -> running
failed -> starting -> running
starting -> failed
```

Successful completion is the same as create: the sandbox ends in `running` and
the active operation is cleared.

### Stop

```text
pending -> stopping -> stopped
provisioning -> stopping -> stopped
starting -> stopping -> stopped
running -> stopping -> stopped
failed -> stopping -> stopped
stopping -> failed
```

Successful completion:

```text
phase=stopped
activeOperation=null
lastOperationStatus=success
errorMessage=null
```

### Restart

Restart is modeled as one operation because it is one user intent, even if the
runner internally performs stop then start.

```text
running -> stopping -> starting -> running
failed -> starting -> running
stopping -> failed
starting -> failed
```

Successful completion:

```text
phase=running
activeOperation=null
lastOperationStatus=success
errorMessage=null
```

### Delete

Delete is terminal.

```text
pending -> deleting -> deleted
provisioning -> deleting -> deleted
starting -> deleting -> deleted
running -> deleting -> deleted
stopping -> deleting -> deleted
stopped -> deleting -> deleted
failed -> deleting -> deleted
deleting -> failed
```

Successful completion:

```text
phase=deleted
activeOperation=null
lastOperationStatus=success
errorMessage=null
```

After `phase=deleted`, the resource may remain as a tombstone for list-watch
consumers or may be hard-deleted after a retention period. Hard deletion must
emit a final project resource event before the row disappears.

## Failure And Retry

Any operation may fail from a transitional phase. Failure should preserve user
intent while making the failure visible.

```text
phase=failed
activeOperation=null
lastOperationStatus=failed
errorMessage=<failure detail>
```

Retry behavior:

| Desired state | Retry operation |
| --- | --- |
| `running` | `start` or `restart`, depending on whether provider resources exist. |
| `stopped` | `stop`. |
| `deleted` | `delete`. |

The runner should not invent a new desired state during retry. It should resume
work toward the existing `desiredState`.
