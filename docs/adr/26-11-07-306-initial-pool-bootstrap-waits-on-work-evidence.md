# 26-11-07-306 — Initial pool bootstrap waits on work evidence

- **Status**: Proposed (narrows [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md) §6's pool-host ceiling for actions waiting on initial pool bootstrap, and [0039](0039-attach-waits-for-readiness-at-every-tier.md)'s tier-1 stall rule for bounded bootstrap operations without incremental progress)
- **Date**: 2026-11-07
- **Issue**: [#772](https://github.com/discobox-ai/discobox/issues/772)
- **Preserves**: [26-11-04-167](26-11-04-167-an-explicit-start-waits-for-its-pool-to-return.md) §2 and [26-11-05-143](26-11-05-143-a-start-waits-for-a-compatible-pool-and-its-pending-upgrade.md) §4's explicit-start and upgrade ceilings, protocol discovery and execution fencing; source-delivery gates and settled-failure semantics.

## Context

The issue reports a cold Docker first attach returning while its pool is still
registering. The roughly 310 seconds between server readiness and attach exit
are consistent with the five-minute ceiling, but are not a reproduction or
proof of a release performance regression. Heavy host load and downloads are
confounders. Successful apt refresh distinguishes this report from the earlier
missing-proxy failure.

Checked against `main` at `c106cf5358d9935796db8fddc179435ba8f45d5e`:

- `server/internal/resources/sandboxes/service.go` rejects acquisition when
  the assigned pool is not ready. `attach_wait.go` starts `poolSince` on
  `ErrPoolNotReachable`, including the initial `registering` state, and ends
  the action at `sandboxPoolHostWaitCeiling` despite changing progress.
- `server/providers/poolruntime/provider.go` independently caps
  `awaitPoolAgentClient` at five minutes. `schedulablePool` and `waitForPool`
  also wait on readiness, using `ProvisionProgressAt`. Fixing only attach
  leaves another wait able to settle the sandbox as failed first.
- `server/providers/dockerworker/image_load.go` observes archive transfer and
  extraction counters. The engine's `createPoolContainer` holds a display
  phase while `poolruntime.ProvisionBox` applies bundles, before the pool
  agent can serve. `box.go` already bounds that complete apply at 15 minutes.
- `server/internal/resources/pools/reconciler.go` marks `registering` before
  image preload and replacement. It separately repairs a never-registered
  runtime after `poolRegistrationTimeout`. Registration state alone cannot
  distinguish active bootstrap from an abandoned host.
- Routine reconciles and held display phases update provisioning timestamps.
  The action and provider ceiling regressions deliberately protect against a
  permanently unreachable host refreshing its own stall budget.

The decision is about evidence and ownership of a wait, rather than a larger
constant or another CLI reattach loop.

## Decision

### 1. Initial bootstrap is an explicit, one-way entitlement

The pool control plane owns durable initial-bootstrap provenance: its attempt
identity, target generation, active operation, operation start/deadline and
accepted forward-progress marker. Successful first registration permanently
ends eligibility for this pool. A later outage, repair, upgrade or replacement
of a once-registered pool is recovery, even when its state says `registering`.
Neither a state string nor absence of a recent heartbeat establishes eligibility.

New pools establish provenance before starting bootstrap. Existing rows may
qualify only with durable evidence of a never-registered initial attempt;
ambiguous legacy rows use the existing bounded policy. Use additive migration,
retain databases, and preserve the metadata across unrelated whole-row writes.
A stale reporter cannot update a newer attempt or generation.

Only action readiness waits and the initial sandbox create's pool-readiness /
compatible-client acquisition use this entitlement. An explicit start, including
a repair's final start, keeps its existing absolute ceiling. No new retry is
added to restart, scheduled wakes, reads, inspects or observes.

### 2. Count work, and bound work that cannot report movement

Initial bootstrap has no five-minute overall pool-host ceiling. Its default
silence budget is two minutes. Renew it only on new, accepted work evidence
from the current attempt: increasing bytes/layers within an identified image
or bundle operation, a completed provisioning step, or a one-way transition to
the next real operation. Seed the waiter from the current marker; merely
reading an old marker is not progress.

Repeated phase reports, held-phase timer ticks, health probes, heartbeats,
log output, row timestamps, retry counts and reconcile scheduling never renew
this budget. Counter resets, phase oscillation and replayed completion reports
also do not count. Identify each operation and retain its high-water mark;
re-entering an operation cannot manufacture a new budget.

Some bootstrap operations expose no reliable incremental evidence. They get a
nonrenewable operation deadline, recorded before dispatch, of at most 15
minutes. In particular, the existing 15-minute `ProvisionBox` apply is one
such operation, not a fresh allowance for each apt log line or feature step.
While that operation is active, its fixed deadline replaces the two-minute
silence bound. Polling it or restarting its worker never moves that deadline.
A streaming transfer uses the two-minute silence budget; it may exceed five
or fifteen minutes while real counters keep advancing.

Starting a bounded operation requires evidence from its executing provider,
not a waiter declaring that a host must be busy. Providers must implement the
required reporting contract; an unsupported or unobservable stage falls back
to the two-minute silence rule. Reporting must work before pool-agent readiness,
through the server-side bootstrap executor's existing guest transport. Do not
wait for a heartbeat to obtain the very evidence needed to wait for bootstrap.

### 3. The same evidence governs every bootstrap gate

Action acquisition, placement/readiness waits, `awaitPoolAgentClient`, and
pool registration expiry must consume the same authoritative bootstrap facts.
Do not let an inner five-minute ceiling terminate an eligible bootstrap, or
let registration expiry repair a runtime still executing valid bootstrap work.
After apply completes and its agent is launched, start the existing two-minute
first-registration allowance once, from that completion rather than the earlier
`registering` state transition. Persist this deadline across polls and restarts;
a repeated completion report cannot move it. Retain the existing shorter
agent-health check in parallel. Missing registration or failed health ends the
attempt at its applicable deadline; it cannot grant another apply window.

Expiry invalidates the bootstrap entitlement for that attempt and returns a
stall or operation-timeout reason. Automatic reconcile retries and runtime
replacement cannot mint a fresh entitlement and hold the original command
forever. Explicit new lifecycle intent may admit a new attempt, but an existing
wait does not silently follow it. A caller encountering a newer attempt ends
with a conflict. This does not add retries of settled failures or cancel
provisioning merely because an attach caller leaves.

Once initial registration succeeds, actions use the ordinary sandbox readiness
policy. If the now-registered pool becomes unreachable, recovery starts its
existing five-minute ceiling; it cannot regain initial-bootstrap eligibility.
Protocol discovery remains mandatory before create. A reachable incompatible
agent is a protocol/recovery transition under the existing ceiling, not proof
of initial bootstrap work. Lost or unanswered dispatched creates retain their
existing resend budget and ambiguity handling.

The caller's deadline and cancellation always apply. Bound each probe/RPC by
the current remaining allowance and release leases between attempts. Preserve
terminal bootstrap errors immediately, including failed apply, missing image,
exited runtime and authorization/protocol errors. A wait timeout reports the
pool, operation and last meaningful refusal; it must not describe mere
`registering` as a terminal provisioning failure. Settled pool failures must
reach action waiters even if their sandbox row has not yet recorded them.

### 4. Show the work while the original command waits

Use existing provisioning status and request-progress transport to show the
pool and current bootstrap operation, replacing it as the command moves to
sandbox or harness provisioning. Clear status on every exit. Noninteractive
stderr stays bounded and JSON results remain parseable. Display updates do not
count as work evidence. The original `run` reaches its harness without a
second attach command; there is no client-side command replay.

## Alternatives rejected

- **Raise the five-minute constant.** It moves the failure to a slower host
  and lengthens ordinary recovery without distinguishing work from liveness.
- **Remove the ceiling and watch `ProvisionProgressAt`.** Reconciliation and
  held phases can refresh it forever without completing anything.
- **Require byte progress during every operation.** Apt, extraction and guest
  execution can legitimately be silent. Fixed operation deadlines handle that
  uncertainty without treating silence as unlimited progress.
- **Give every registering pool the new policy.** Replacement also registers;
  it would reverse the accepted recovery and explicit-start bounds by accident.
- **Retry attach in the CLI.** It hides a server policy conflict, does not fix
  provider waits, and makes other action clients behave differently.

## Consequences and acceptance evidence

Bootstrap gains durable provenance and evidence separate from display telemetry.
Silent operations can still time out under extreme load: 15 minutes is a
bounded allowance, not a claim that a process is advancing. Revisit that bound
only with measured cold-bootstrap evidence showing valid silent operations
exceeding it; do not increase it based on recurring phase stamps.

Implementation must test both action and provider waits beyond five simulated
minutes with real progress, and through a silent apply within its fixed bound.
Test frozen counters, repeated logs/phase holds/reconcile stamps, old markers,
counter resets, stale attempts, restart, deadline expiry, cancellation and
terminal failures. Assert registration repair cannot interrupt a valid apply,
that an apply exceeding two minutes still permits a heartbeat within its
subsequent registration window, and that retries cannot reset an expired attempt.
Cover first registration
followed by outage, incompatible protocol, ambiguous legacy metadata, safe
migration and the unchanged explicit-start/upgrade and unanswered-create limits.

Runtime acceptance remains the issue's cold nested Docker launch under load,
with the required proxy: the original invocation reaches Claude Code's first
screen without reattach. Retain command, pool and provisioning logs, capture
T1/T2/T3 with their measurement definitions and warm timings, and demonstrate
a stalled and a terminally failed bootstrap produce their concrete reasons.
This ADR claims no runtime validation or performance improvement.

Metadata layout, operation identifiers and reporting transport details remain
implementation choices within these rules. Update live design documents with
the implementation; this proposal changes no runtime behavior.
