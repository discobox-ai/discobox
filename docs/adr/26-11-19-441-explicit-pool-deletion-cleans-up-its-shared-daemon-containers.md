# 26-11-19-441 — Explicit pool deletion cleans up its shared-daemon containers

- **Status**: Proposed
- **Date**: 2026-11-19

## Context

Deleting the last Docker pool in a project can leave its snapshot-build
containers running on the host (#1404). The agent can also leave sandbox
containers behind. The sandbox assignment check in
`server/internal/resources/pools/reconciler.go` protects boxes the control
plane still tracks. It cannot account for leftover Docker containers.

The Docker worker's `RemovePool` in `server/providers/dockerworker/engine.go`
removes the pool container, console and network. The local driver's
`DeleteVM` in `server/providers/docker/local.go` leaves the shared daemon
running. `SyncKnownPools` in `pool-agent/sandboxruntime/runtime.go` can reap
agent-created containers for unknown pools, but it needs another live
agent in the same project. The local provider's drift watcher owns orphaned
pool runtimes and can recover a pool container missed during deletion.

## Decision

The provider owns removal of agent-created Docker containers during
explicit deletion on a shared daemon. The reconciler still checks for
assigned sandboxes before asking the provider to remove the pool. Stop and
remove the pool runtime first so the agent cannot start new containers
during cleanup. Then remove its sandbox and snapshot-build containers
before removing their network or any pool state trees that teardown owns.
On a dedicated per-pool daemon, destroying the VM disposes of its containers
and remains the cleanup boundary.

Use the deleting pool row's project ID and pool ID as the authority. Enumerate
running and stopped containers on the pool's daemon. Require exact matches
for both `discobox.project_id` and `discobox.pool_id`, and verify that each
candidate belongs to one of the two existing container kinds:

- Sandboxes carry `discobox.sandbox.managed=true`.
- Snapshot builds have a name beginning with
  `discobox-snapshot-<pool-id>-`, the naming convention in
  `pool-agent/sandboxruntime/snapshotdocker.go`.

Remove confirmed candidates by Docker container ID. Containers with missing or
mismatched identity, including sibling pools in the same project, are outside
the deletion scope. The drift watcher keeps its pool-runtime scope and
`pool-sync` remains the backstop for unknown pools. Volume and state-tree
ownership is covered separately by #32.

A daemon connection failure, list failure or failed removal on the shared
daemon fails the provider delete. Docker's container-not-found result on
removal counts as success, including when another cleanup removed the same
container. The existing pool reconciler records the failure, retains the
row and delete intent, and retries with backoff. Each attempt repeats the
scoped cleanup. This makes partial cleanup safe across errors and server
restarts. After successful teardown, the reconciler hard-deletes the row
as before ([ADR 0010](0010-deletes-are-hard-deletes.md)).

## Alternatives considered

- **Ask the departing agent or a sibling agent to clean up.** A live agent
  can reuse its sandbox cleanup logic, but a crashed agent cannot answer
  and the last pool has no sibling. Making delivery of that request durable
  still leaves cleanup without an executor in those cases.
- **Extend the drift watcher to reap agent containers after the row is
  gone.** This would let deletion complete while Docker is unavailable, but
  the watcher would have to infer ownership from absence, without the
  deleting row's project and pool identity. It also makes cleanup failure
  invisible to the delete operation.
- **Remove every container with the pool ID label.** That catches both kinds
  with one query, but it treats a generic ownership label as authorization to
  delete any container, including kinds with separate teardown rules.

## Consequences

A pool deleted during a shared Docker daemon outage stays pending cleanup
until the daemon recovers. Operators can see the cleanup failure on the pool
while the reconciler retries. The provider and agent must agree on the
existing container identity conventions. The implementation may choose how
to share those conventions and how many Docker queries to use.
