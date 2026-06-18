# Manager Design

`manager` owns the hook-domain manager used by the daemon. It sits between the
socket API adapter, the API-oriented `service` package, and the daemon runtime
loops.

## Responsibilities

- Own the discovered hook set used for hook existence lookups.
- Own runtime hook execution state that is shared by status reporting and queue
  draining, such as whether a hook is currently running and which phases are
  currently active for queue draining.
- Coordinate API-triggered hook-domain side effects:
  - pause/resume audit events
  - manual run audit events
  - phase activation for phase-targeted manual runs
  - wakeups for the daemon drain loop
  - shutdown cancellation requests
- Expose API-shaped application operations by delegating durable reads and writes
  to `service`.
- Provide small runtime helpers used by daemon loops, such as global pause checks
  and audit event recording.

## Boundaries

- Do not own HTTP routing, OpenAPI request/response conversion, Unix socket
  setup, or SSE transport behavior; those stay in `daemon`.
- Do not own raw persistence; use `store`.
- Do not own API DTO translation rules that already belong in `service`.
- Do not run hook processes or watch the filesystem directly; the daemon runtime
  still composes `watcher`, `matcher`, and `runner`.

The package is intentionally a first extraction point from the daemon. Runtime
orchestration can move here incrementally when it becomes independent of socket
and process lifecycle concerns.
