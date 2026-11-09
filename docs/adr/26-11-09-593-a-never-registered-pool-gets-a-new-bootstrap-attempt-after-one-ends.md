# 26-11-09-593 — A never-registered pool gets a new bootstrap attempt after one ends

- **Status**: Proposed (narrows [26-11-07-306](26-11-07-306-initial-pool-bootstrap-waits-on-work-evidence.md) §3's "automatic reconcile retries … cannot mint a fresh entitlement" to the waiters holding the ended attempt)
- **Date**: 2026-11-09
- **Issue**: [#866](https://github.com/discobox-ai/discobox/issues/866)
- **Preserves**: 26-11-07-306 §§1–2 and 4, and §3's rules for waiters (pinned to one attempt, ended by expiry, by a terminal error, or by a newer attempt, and never following a new one);
  [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) §4's settled-failure semantics for registered pools;
  the "terminal `failed` only for never-registered pools" rule in `server/providers/DESIGN.md`.

## Context

Checked against `main` at `b6c595c642db693c6123388ccf55765327591d23`:

- `Pool.BeforeCreate` (`server/internal/model/model.go`) is the only place a
  bootstrap attempt is created. Its two-minute silence budget starts at row
  creation, and the row stores it, so it keeps running across server restarts.
- `PoolReconciler.reconcileActive`
  (`server/internal/resources/pools/reconciler.go`) has two paths that apply
  to a never-registered pool whose attempt belongs to the current
  generation. If the attempt has expired, the reconciler records the
  `Failure` and returns without calling the provider. If the provider returns
  an error, the reconciler records that error as the attempt's `Failure`
  through `sandbox.ReportBootstrap`.
- `PoolBootstrap.Expired` and `Accept`
  (`server/internal/model/pool_bootstrap.go`) treat a recorded `Failure` as
  permanent. The engine keeps retrying with backoff (2s, doubling to 5m), but
  every retry stops at the top of `reconcileActive` with the same stored
  failure. The pool stays `failed` until someone re-creates its row.
- `registrationExpired` returns false while the pool has an attempt, so the
  existing repair of a never-registered runtime doesn't run either.
- A generation bump, such as `admin pool update` or `SchedulePoolRepair`,
  leaves the attempt at the old generation. `BootstrapFacts` stays non-nil,
  but every check also requires `b.Generation == generation`, so the pool
  drops back to the legacy registration-timeout policy. Nothing creates an
  attempt for the new generation, so a pool that never registered loses its
  work-evidence waits for the rest of its life. Nothing in production calls
  `SchedulePoolRepair` today, and no user-facing command means "retry this
  pool".
- Waiters (`awaitUsable` in `sandboxes/attach_wait.go`, `poolWaitBudget` in
  `providers/poolruntime/provider.go`) pin themselves to one attempt through
  `BootstrapWait`. When the attempt changes, they end with "bootstrap attempt
  changed". The acquisition gate in `sandboxes/service.go` refuses an action
  with the attempt's failure once the attempt has expired.

The issue's trigger is the dev loop. The server's own `Default` pool
(`service/defaults.go`) used up its whole budget while the server
crash-looped during its first two minutes, and stayed `failed` for good. No
command was waiting on it. The rule 26-11-07-306 §3 protects is about the
waiter. The implementation applied that rule to the pool's reconciler, so
the pool itself can never recover.

## Decision

### 1. An ended attempt does not end the pool

For a pool that has never registered (`RegisteredAt == nil`) and has an
attempt on record, the reconciler starts a **new attempt** on the next pass
after the current attempt has ended. An attempt has ended when either of
these holds:

- it is finished: it has a recorded `Failure` (expiry, provider error, or
  executor-reported failure);
- it is stale: it belongs to an earlier generation than the pass being
  reconciled.

A new attempt has a fresh random identity, the generation of the pass, and a
`ProgressAt` of now. Everything else is empty. One fenced store write
replaces the attempt, and it accepts only if the attempt it read is still the
one on the row. Every place an attempt can come from keeps the 26-11-07-306
§1 evidence rule. A row with no bootstrap document (an ambiguous legacy row)
never gets one this way and keeps the existing bounded policy. A registered
pool never gets an attempt again.

This makes stale generations follow the same rule as failures. New intent on
a never-registered pool (an update, a repair) now gets its own attempt with
work-evidence waits, as §3's "explicit new lifecycle intent may admit a new
attempt" anticipated, and the pool no longer drops silently to the legacy
policy.

### 2. The failing pass settles, and the engine's backoff paces the next one

Finishing an attempt and starting the next one are separate passes:

- The pass that sees expiry, or gets a provider error, records the
  `Failure`, shows the pool as `failed` with the reason (`failReconcile`, as
  today), and returns the error. The engine's existing failure backoff paces
  the retry: 2s doubling to a 5m ceiling, with no new constants.
- The next pass finds the finished attempt, starts a new one, sets the pool
  back to `pending`, and calls the provider.

This brings back what `failed` meant for a never-registered pool before
work-evidence waits existed: the last try failed, and another one is
scheduled. It does not mean the pool will never be tried again. Drift marks
still don't shorten the backoff. New intent still does.

**No attempt cap.** A never-registered pool owns no volumes and no
sandboxes, so a retry puts nothing at risk. At the ceiling it costs one
provider call every five minutes. A cap would bring back the latch this ADR
removes, just later.

**All failures are retried**, including the ones §3 calls terminal for a
waiter (a missing image, an exited runtime, an authorization or protocol
refusal). Those errors still end the **waiter** immediately, as §3 requires.
But their causes are often fixed outside the pool, which is what happened in
the issue: the image watcher finished its first build, and a credential or
an image push can do the same. The backoff ceiling bounds the cost of
retrying a cause that never gets fixed. `ErrPoolConfigApply` keeps its
existing path. It already settles without a bootstrap failure and is
re-driven the same way.

### 3. A new attempt replaces what the last one left

The first provider call of a new attempt is `RepairPool`, not
`ReconcilePool`. It replaces the runtime under the same pool identity, the
way the legacy expired-registration path does. The runtime that the ended
attempt left behind (a half-pulled image, a container whose agent never
called home, an apply cut off by a dead server) is not trusted. Reconciling
it in place would find a host that is "coming up" (`ErrPoolNotReachable`),
requeue, and let the new attempt expire on the same stuck runtime. The pool
has never registered, so the repair has nothing to preserve.

### 4. Waiters still end, and never follow

Nothing changes for a waiter:

- A command waiting on the ended attempt ends with that attempt's expiry or
  terminal reason, or with "bootstrap attempt changed" if it next sees the
  new attempt. §3's conflict rule applies.
- While the pool is backing off, the acquisition gate refuses a new action
  with the finished attempt's reason, as it does today.
- An action that arrives after a new attempt has started waits on that
  attempt, from its current marker, under the §2 rules.

Automatic retries therefore still never hold the original command, which is
what §3's sentence was there to prevent.

### 5. The budget still starts at creation

The attempt's silence budget still starts when the attempt is created, and
it keeps running across server restarts. A restart that kills an attempt's
work has stalled that attempt, and §§1–3 recover it. In the dev loop, a
crash-looping first boot now costs one silence budget plus one backoff step
before a fresh attempt begins. It no longer costs the pool.

## Alternatives rejected

- **Keep the latch, and add `admin pool repair`/`retry` as explicit intent
  (issue option b).** This fits §3 as written. But the server's own
  `Default` pool, which no one asked for, would stay dead after every slow
  first boot until someone notices and runs a command, and the dev loop hits
  that on most cold starts. A command can be added later as a way to skip
  the backoff. It is not how a pool recovers.
- **Restart the budget when a new server process takes over the attempt, or
  start it at first dispatch (issue option c).** This fixes only the
  restart trigger. One cancelled pull or one real stall still latches the
  pool. With §1, the restart case already recovers, at the cost of one
  budget, so a second way to start an attempt (process identity on the
  attempt) adds a mechanism without fixing anything more. Revisit only if
  measured cold starts show that delay matters.
- **Retry only expiry and transient errors.** Telling "transient" from
  "hard" needs a classification the providers don't make reliably. The
  issue's own failure was a "hard" missing image that a build later
  provided. Waiters already get hard errors immediately, so the pool gains
  nothing by keeping them forever.
- **Cap attempts, then stop.** That is a delayed latch. Nothing is at risk
  on a never-registered pool, so stopping protects nothing.
- **Start the new attempt on the same pass that recorded the failure.**
  That skips the engine's backoff, so a failure that comes back immediately
  would spin. It would also show `failed` only for an instant, so no one
  would see it.

## Consequences and acceptance evidence

`failed` on a never-registered pool goes back to meaning "the last attempt
failed, and another is scheduled". A pool is never `failed` for good unless
its cause never clears, and then it costs one attempt every five minutes.
The workaround (delete and re-create the pool) is no longer needed. A
generation bump on a never-registered pool now brings its work-evidence waits
back, instead of dropping it to the legacy policy.

Implementation must test:

- An expired attempt, and one with a provider error, followed by a
  successful retry that clears the failure and reaches `active`.
- The backoff between attempts, with no attempt started on the failing pass.
- The fenced attempt replacement losing to a concurrent replacement or to
  new intent.
- A stale-generation attempt replaced for the new generation.
- A legacy row without a bootstrap document, and a registered pool: neither
  gets an attempt.
- `RepairPool` as the first call of a new attempt.
- A waiter pinned to the old attempt ending, and never following the new one.
- An action arriving during a new attempt waiting on it.
- Executor reports for the old attempt refused after the new one starts.

Runtime acceptance is the issue's scenario. Run a cold `task dev` whose
server crash-loops past the Default pool's first silence budget. The pool
shows `failed`, then reaches `active` without being deleted, once the server
stays up. Separately, a pool whose cause never clears settles into one
attempt per backoff ceiling.

Open for implementation, within these rules: how the bootstrap document
records the attempt it replaced, if at all; whether `pool get` and the
console show when the next attempt is due; and how the store writes the
attempt replacement. Update `server/internal/resources/pools/DESIGN.md`
("Initial bootstrap waits") and `server/providers/DESIGN.md` ("Pool Failure
and Recovery") together with the code. This proposal changes no runtime
behavior.
