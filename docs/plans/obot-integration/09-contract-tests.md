# WI-09 — Managed contract test suite

**Goal:** a test suite that proves the Discobox half of the managed contract
behaves as an upstream controller assumes, especially under retry, loss, and
out-of-band change.

Read `00-CONTEXT.md` first. **Blocked on WI-03's API shape being frozen** — but
the suite can be written against the OpenAPI contract before the implementations
land, and should be, because a test written after the fact tends to encode
whatever the code already does.

## Why

Every hard case in this integration is a distributed-systems case: a lost
response, a duplicated delete, a runtime that disappeared, an event delivered
twice. None of them show up in ordinary development, and all of them show up in
production. The upstream ADR lists the scenarios both sides must cover; this
item owns the Discobox half.

## Current state

- `test/` holds integration and Bats tests.
- `server/internal/service/docker_pool_flow_integration_test.go` and
  `server/providers/poolruntime/pool_docker_e2e_test.go` are the closest
  existing end-to-end patterns — read them before choosing a harness.
- Store-level fixtures exist, e.g. `server/internal/store/pool_fixture_test.go`.
- `go tool task test` runs root-module tests; `go tool task test:all` includes
  nested modules.

## Scenarios to cover

Idempotency and identity:

- create response lost, then the same `PUT` retried — one resource, not two;
- same revision key with the same payload — accepted and converged;
- same revision key with a *different* payload — accepted, not rejected (the
  revision is correlation metadata, not an integrity constraint);
- a changed revision requiring an in-place update;
- a changed revision requiring concrete sandbox replacement, with the managed
  identity preserved across it;
- out-of-band loss of the concrete runtime, followed by convergence;
- managed-pool creation retried after an ambiguous response.

Deletion:

- asynchronous deletion: `202`, then still-exists, then not-found;
- repeated `DELETE` continues the same deletion and never starts a new
  generation;
- managed-pool deletion refused while managed sandboxes remain assigned;
- ordering: sandboxes deleted and finalized before the pool that hosted them.

Events:

- delayed and duplicate sandbox events are harmless;
- event-stream reconnect delivers a fresh snapshot that closes the gap.

Capacity and QoS (pairs with WI-06):

- launching many sandboxes beyond the pool's physical capacity, with no
  reservation-based admission rejection;
- pool pressure, QoS action, and sandbox termination are reported;
- an explicitly confirmed live-pool envelope reduction applies.

Utilization (pairs with WI-07):

- a partial snapshot when one sandbox cannot be sampled;
- a utilization request while the pool is offline returns explicit unavailable;
- utilization reads create no project events and no persisted status churn.

Secrets:

- rotating the value behind a stable secret ID refreshes the sandbox's secret
  channel **without** changing the managed configuration revision, **without**
  restarting the sandbox, and **without** exposing the value in any status,
  response, or log line.

## Out of scope

- Upstream's half of the contract. Obot runs its own suite against an HTTP fake
  and a real Discobox server.
- Load and performance testing.
- Testing behavior WI-01 has not yet decided. If a scenario depends on an
  unsettled decision, write the test and skip it with a reference to the open
  question rather than guessing.

## Design questions for the engineer

- **What level does each scenario belong at?** Response loss and retry are
  cheap at the service/store level; runtime disappearance and pool pressure
  need a real Docker-backed pool. Splitting by cost keeps the fast suite fast.
- **How is response loss simulated?** Killing the connection after the server
  commits is the honest version; calling the service twice tests less but costs
  far less.
- **How is out-of-band runtime loss induced?** Deleting the container behind the
  pool agent's back is realistic; a store-level fixture is faster.
- **Are these gated in CI or run on demand?** Docker-backed end-to-end tests are
  slow. Check how the existing e2e tests are gated before adding more.

## Done when

- Every scenario above is covered or explicitly skipped with a written reason.
- The suite runs under `go tool task test:all`, with slow tests gated the same
  way the existing e2e tests are.
- A deliberately introduced idempotency regression in the managed `PUT` path
  fails the suite.
- `go tool task check-hooks` passes.
