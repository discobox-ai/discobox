# Orchestration Design

This package coordinates desired-state resource updates with durable reconcile
jobs.

API handlers record intent on resource rows, persist a resource event, and ensure
a durable reconcile job. Jobs reconcile one resource generation. If new intent
arrives, the old job cancels and a newer pending job handles the new generation.

## Resource Shape

Orchestrated resources embed `model.ResourceLifecycle` and implement project
event identity:

```go
func (r *Example) EventProjectID() string { return r.ProjectID }
func (r *Example) EventResourceType() string { return "example" }
func (r *Example) EventResourceID() string { return r.ID }
```

Operation specs describe accepted user intent, not rigid FSM rules.

## Transaction Shape

`Begin` runs the accepted-intent write in one transaction:

1. Prepare/load the resource.
2. Increment generation.
3. Apply operation spec.
4. Ensure a durable reconcile job for the new generation.
5. Set `lastJobId`.
6. Persist the resource and resource event.
7. Commit, then notify the dispatcher as a wakeup optimization.

The dispatcher notification is not the source of truth; durable rows are.

## Job Semantics

- Job payloads carry resource IDs and the accepted generation.
- Reconcile jobs reload the resource with generation awareness.
- Generation conflicts cancel superseded jobs without retry.
- Pending jobs for the same resource may be coalesced to the latest payload.
- Running jobs do not block newer intent from creating a newer pending job.
