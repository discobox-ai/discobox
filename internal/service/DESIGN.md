# Service Design

This package implements API-facing business logic, sandbox orchestration wrappers,
reconcilers, and provider-facing sandbox operations.

## Sandbox Lifecycle

Sandbox state is modeled as desired-state reconciliation. The API records the
user's desired steady state, the currently observed phase, and the latest user
intent that requested reconciliation.

| Field | Meaning |
| --- | --- |
| `desiredState` | User intent: `running`, `stopped`, or `deleted`. |
| `phase` | Observed lifecycle phase displayed to clients. |
| `activeOperation` | Operation currently queued or running. |
| `lastOperationStatus` | State of the latest lifecycle operation/job. |
| `generation` | Monotonic desired-state generation. |
| `observedGeneration` | Latest generation fully handled by reconciliation. |
| `restartGeneration` | Monotonic user intent counter for restarts. |
| `restartedGeneration` | Latest restart generation completed by reconciliation. |

## Desired States and Phases

Desired states:

| Value | Meaning |
| --- | --- |
| `running` | Sandbox should eventually be running. |
| `stopped` | Sandbox should eventually be stopped but retained. |
| `deleted` | Sandbox should eventually be removed. |

Phases:

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

## Operation Intent

| API action | Desired state | Initial phase | Active operation |
| --- | --- | --- | --- |
| create | `running` | `pending` | `create` |
| start | `running` | `starting` | `start` |
| stop | `stopped` | `stopping` | `stop` |
| restart | `running` with `restartGeneration++` | `starting` | `restart` |
| delete | `deleted` | `deleting` | `delete` |

Restart is not a steady desired state. It is represented by incrementing
`restartGeneration` while keeping `desiredState=running`.

## Service Responsibilities

Service methods should:

1. Validate parent resources and request shape.
2. Build or load the model.
3. Delegate lifecycle intent changes to the resource-specific orchestrator.

Reconciliation coordination and provider mechanics are separate:

- `SandboxReconciler` loads generation-scoped resources, writes lifecycle
  progress, maps generation conflicts to canceled jobs, and chooses the operation.
- `SandboxOperations` performs provider/domain work such as start, stop, restart,
  and delete. It should not know about API DTOs, resource events, or orchestration.
