# 26-11-15-829 — A scheduled wake is kept until its pool takes the start

- **Status**: Proposed (would supersede [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md) §6's claim before the send and its "the start is sent once", and its rejected alternative "Wake retried until it succeeds" for a start the pool never took; would supersede [26-11-04-167](26-11-04-167-an-explicit-start-waits-for-its-pool-to-return.md) §1's "The power reconciler's scheduled wake remains single-send, including when the pool is unreachable", and narrow [26-11-05-143](26-11-05-143-a-start-waits-for-a-compatible-pool-and-its-pending-upgrade.md)'s preservation of single-send scheduled wakes to match)
- **Date**: 2026-11-15
- **Issue**: [#1192](https://github.com/discobox-ai/discobox/issues/1192)
- **Preserves**: 26-10-16-337 §§1–5 and 7–8 (sleep and wake files, confirmation, handover at boot), §6's `wakeAt`/`wakeEpoch` columns, their guarded writers and cancellation by every outside instruction; [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) §9 (a start is an instruction, never desired state); 26-11-04-167 §§2–4 for explicit starts; [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md)'s refusal to loop on a start the runtime answered with a failure; [26-11-14-624](26-11-14-624-after-an-upgrade-the-server-is-not-ready-until-its-pools-are.md) whole.

## Context

Boxes that slept with `discobox-power sleep --wake-in …` across a server
restart did not wake at their time (#1192). Nothing woke them until a person
started each one by hand.

The timer itself survives a restart. Checked against `main` at
`88086c6e4e2114bb28b0f809cc9ccfedd3849e26`:

- The wake is durable twice over: `sandboxes.wake_at`, written by
  `Store.SetSandboxWakeIfEpoch` (`server/internal/store/sandboxes.go`), and
  the `sandboxPower` row in `reconcile_dirty`, whose `not_before` the power
  reconcile sets with `Result.RequeueAt(wakeAt)`
  (`server/internal/resources/sandboxes/power_reconcile.go`).
- What loses it is the send. `sandboxPowerReconciler.wake` first claims the
  wake, clearing `wake_at` under the epoch it read, and only then resolves
  the provider and sends one start through `instructSandbox`
  (`server/internal/resources/sandboxes/power.go`), the unfenced start
  (`ref.StartEpoch` nil). A failure is logged as `wake sandbox` and the
  reconcile returns success. Nothing retries it.
- After a server start, `Store.BeginPoolHealthChecks` invalidates every
  pool's last report, so every pool reads `unknown`
  (`model.Pool.Health`, `server/internal/model/pool_health.go`) until its
  agent sends a fresh heartbeat. A VM pool's agent is not reachable until its
  machine boots again. A wake that fell due while the server was down is
  claimed as soon as the engine starts. Its start fails with
  `sandbox.ErrPoolNotReachable` (`poolAgentLostError`,
  `server/providers/poolruntime/provider.go`), and the wake is gone.
- A wake whose start is in flight when the old server stops is lost the same
  way: its context is canceled after the claim. A crash between the claim and
  the send loses it too.
- An explicit start already has an epoch-fenced route.
  `poolAgentClient.Start` (`server/providers/poolruntime/agent_client.go`)
  sends `PoolStartAdmittedSandbox` with `StartEpoch`. The pool agent calls back
  `AuthorizePoolSandboxStart`
  (`server/internal/resources/pools/agent_service.go`) under its per-sandbox
  power lock, and that refuses the start unless `wake_epoch` still equals the
  epoch and the sandbox is present. An old agent without the route answers
  404, which the client reports as `ErrPoolNotReachable` and never falls back
  to the unfenced start. Every sandbox provider's `Start` is the pool
  runtime's.

26-10-16-337 §6 and 26-11-04-167 §1 made the wake single-send. 167 rejected a
retry because it would replay scheduled wakes after a later stop, and hold
power reconcile slots while a host is absent. 337 followed 711: a start that
keeps failing becomes a loop nobody can see. The owner chose on #1192: `wake_at`
is not cleared until the wake succeeds or an outside instruction clears it,
and a wake is not sent while its pool is not healthy.

## Decision

### 1. The wake is spent when the pool takes its start, not before

The power reconcile no longer claims a due wake before sending. It reads
`wake_at` and `wake_epoch`, sends the start (§3), and clears `wake_at` only
after the pool took it. The clear is conditional on the epoch it read and on
the `wake_at` it read, so it never clears a cancellation's result or a later
wake.

The pool took the start when it answered it: accepted, or refused with an
answer of its own. A refusal the pool answered spends the wake and is
recorded as any failed start is, as 711 wants: retrying a start the runtime
has refused is the loop 711 rejects.

Every other outcome leaves `wake_at` in place, and the reconcile returns the
error, so the engine's failure backoff (2 s doubling to 5 min) schedules the
next try. That covers:

- `ErrPoolNotReachable`, including a call the agent went away under before
  answering, and an old agent without the fenced route;
- the reconcile's context ending, as when the server stops;
- anything that fails before the send, such as provider resolution or opening
  the secret state.

A crash anywhere in between leaves the wake recorded, and the next server
sends it. The ordering rule the issue asked for, that nothing that can fail
sits between a claim and the send, holds because the claim is now after the
send.

### 2. A wake is not sent while its pool is not ready

Before sending, the power reconcile reads the sandbox's pool. Unless the
pool's health is `ready` (`model.Pool.Health`: a heartbeat from this server
run, reporting ready, within the heartbeat timeout), it sends nothing. It
keeps `wake_at` and returns a requeue on a backstop interval, no longer than
the engine's backoff ceiling. The value is left to implementation. A wait for
the pool is not a failure, so it accrues no backoff, and it holds no reconcile
slot.

When a pool's health becomes `ready`, the server marks the power reconcile of
each present sandbox on that pool whose `wake_at` is due, in the transaction
that records the report, as `UpdatePoolStatus` already marks sentinels
(`MarkPoolSentinelsTx`). The mark pulls the row forward from its backstop or
backoff, so a wake that fell due while the server or the pool was down fires
within a reconcile of the pool coming back. Where exactly the transition is
detected (the status write, or the pool reconcile's promotion) is open to
implementation, as long as a report that changes a pool from not ready to
ready marks its due wakes.

Health is the gate, not 26-11-14-624's settled server. A pool that reports
ready before its agent is replaced can still fail the start; §1 keeps the wake
and retries it under backoff.

### 3. The wake's start is fenced at the pool

The wake's start goes through the fenced route: `instructSandbox` with
`startEpoch` set to the `wake_epoch` the reconcile read, as an explicit start
sends. The pool agent authorizes it under its power lock, against the
current epoch and presence.

- A person's or a lead's start, stop or restart, a successful suspend, an
  archive or a delete moves the epoch, as every outside instruction already
  does. A wake start not yet executed is then refused at the pool, including
  one whose answer was lost and is replayed or retried later. This is the
  execution-boundary fencing 26-11-04-167 §3 requires, applied to the wake.
- The wake's own start does not move the epoch. Its retries reuse the epoch
  read with the `wake_at` they send for.
- A retry that reaches a sandbox an earlier lost attempt already started is a
  start of a running sandbox, the no-op it is today, and spends the wake.

`SandboxRef.StartEpoch`'s "nil for single-send scheduled wakes" no longer
holds. Actions keep their own policy.

### 4. No limit

A kept wake is retried until the pool takes its start, or an outside
instruction cancels it. Archive and delete already cancel it. A pool that
never comes back costs one requeue per backstop interval per sandbox with a
due wake, and no host call. This keeps 337's position that a wake may be any
time and that the agent is woken late rather than never (§7).

### 5. Explicit starts are unchanged

26-11-04-167's request wait, its budgets, its visible progress and its
supersession rules stay as they are. Restart still gains no retry. Only the
power reconcile's scheduled start changes.

## Alternatives rejected

- **Keep the claim before the send, and put the wake back on failure.** This
  was the triage's prototype: on `ErrPoolNotReachable` or cancellation, write
  `wake_at` back under the claimed epoch. It loses the wake on a crash between
  claim and put-back. It needs the put-back to run on a canceled context. It
  needs every fallible step after the claim to remember to restore. Clearing
  after the pool took it has none of these failure points.
- **Retry every failure until the start succeeds.** A start the pool
  answered with a refusal (no container, a failed runtime) is the loop
  26-10-12-711 rejects: it would retry forever on a sandbox that needs repair,
  holding intent nobody can see. The owner's "until the wake succeeds" is read
  as "until the pool takes it".
- **Engine backoff alone, with no readiness gate.** No new hook, but a pool
  that takes two minutes to boot leaves the wake up to about five minutes late,
  and every try is a call into an absent host. The owner chose not to send
  while the pool is not healthy.
- **A short fixed retry while unreachable.** It fires quickly, but it polls a
  host that is gone and spends reconcile slots doing it. 167's objection
  stands.
- **Gate on the server being settled (26-11-14-624).** It holds every pool's
  wakes behind the slowest pool, and does not help a pool that fails after the
  server settles.
- **Retry inside the provider.** Rejected by 26-11-04-167 for the same
  reasons. The provider cannot tell a wake from an explicit start, and would
  hold the reconcile slot waiting on the host.
- **Give up after a bound.** It would drop a wake the agent asked for because
  a pool was down for a long time. Archive and delete already end a wake that
  should not fire.
- **The unfenced start, as today.** A start whose answer was lost could then
  run after a later stop. The fenced route already exists and is what explicit
  starts use.

## Consequences

- A wake that fell due while the server or its pool was down fires shortly
  after the pool's first ready report, instead of never.
- A wake whose pool is never ready stays recorded and is visible as `wakeAt`
  on the status. 337's deferral of listing a stopped sandbox's wake from
  outside is unchanged.
- An old pool agent without `PoolStartAdmittedSandbox` cannot take a wake. It
  is kept until the agent is replaced, as an explicit start waits today.
- The wake path gains a pool read per due wake and a mark per due wake when a
  pool becomes ready. No schema change: `wake_at` and `wake_epoch` already
  exist and keep their guarded writers.
- `server/internal/resources/sandboxes/DESIGN.md` and the comments on
  `sandboxPowerReconciler.wake`, `Provider.instruct` and
  `SandboxRef.StartEpoch` describe the new policy when it lands.

### Acceptance evidence

Implementation tests at the engine level, over one database across two
engines:

- Sleep with a wake. Stop the engine, let the wake fall due, then start a new
  engine with the pool not ready, then ready. The sandbox is started after the
  ready report, and not before.
- Sleep with a wake and restart the engine before it is due. The start is
  sent at the wake time.
- A start the pool answers with a refusal spends the wake, and nothing
  retries it.
- A stop admitted while the wake waits on the pool, or after a lost first
  attempt, wins. The pool refuses the fenced start, and no start runs after
  the stop.
- A server stop with the wake start in flight leaves `wake_at` set.

The issue's runtime acceptance is a real server restart with a box sleeping
on `--wake-in`. This proposal supplies the decision. It does not implement it
or claim that validation.
