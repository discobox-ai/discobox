# 26-11-24-783 — A teardown the pool does not answer in time is not answered, and frees its reconcile

- **Status**: Accepted
- **Date**: 2026-11-24
- Accepted by the owner by merging #1731, 2026-11-25
- **Issue**: [#1728](https://github.com/discobox-ai/discobox/issues/1728)
- **Relates to**: [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §3 (a failure that is not the reconciler giving up does not settle the
  generation), §§9–10 (power operations are instructions);
  [0022](0022-sandbox-deletion-is-archive-then-confirmed-purge.md) §3 (archive
  and delete are answered only when done). Neither is changed.

## Context

At `e6f5ea446`, new boxes on one server stayed `starting` for hours. Their
`sandbox.reconcile` rows were `pending` with 0 attempts while the rows of
boxes archived the day before were `running`. The cause, from the code:

- **One cap for every sandbox reconcile on a node.** The `sandbox` type is
  registered with `SandboxReconcileJobConcurrency` (default 4;
  `server/internal/service/service.go`, `registerReconcilers`;
  `server/internal/config/config.go`). Creates, starts, archives and removes
  all take a slot from it (`server/internal/reconcile/engine.go`,
  `typesWithCapacity`).
- **A reconcile has no deadline.** The engine runs a reconcile under its own
  context, which ends only when the server stops (`engine.go`, `execute`).
  Nothing bounds how long a reconcile may run.
- **Teardown waits for the pool's answer, and the pool waits on a lock.**
  `archiveSandbox` (`server/internal/resources/sandboxes/archive.go`) calls
  `Provider.Archive` → `poolAgentClient.Archive`
  (`server/providers/poolruntime/provider.go`, `agent_client.go`) with that
  context. The agent answers an archive or a delete only when the work is done
  (`pool-agent/server/sandbox_handlers.go`, after ADR 0022 §3). Both take the
  sandbox's power lock before doing anything. That is a plain `sync.Mutex`
  (`pool-agent/sandboxruntime/power.go`, `sandboxLock`), the same lock every
  start, stop and restart holds while it works, and its wait ignores the
  request's context. An archive behind a start that never finishes is never
  answered. Its reconcile holds a slot until the server stops.
- **An unanswered teardown already has a meaning.** `teardownNotAnswered`
  (`archive.go`) leaves the generation unobserved and lets the engine retry
  with backoff when the pool was not reachable or the reconcile's context
  ended. A client-side timeout isn't one of those today:
  `poolAgentUnanswered` (`provider.go`) excludes `url.Error.Timeout()` because
  a create the agent may still be working on is not a lost agent.

With four such archives, no `sandbox` row is claimed on that node again.
`admin job run-now` only re-marks a row (`server/internal/resources/jobs/service.go`,
`RunNow`), so it cannot help. A restart frees the slots, until the same
teardowns are claimed again.

The triage turned up two engine defects that make the stall look stranger
than it is:

- `claimOne` selects candidates with `not_before <= now` but its guarded
  `UPDATE` re-checks only `seq` and the claim. A candidate read before
  `complete` arms a `RequeueAt` can be claimed after it, so a row whose next
  run is the archive-retention deadline (24 h out) runs, and reads `running`,
  now.
- `leaseLoop` renews every row `claimed_by` this worker, whether or not a
  reconcile for it is in flight. `complete` and `release` ignore their
  write's error. A settle that fails leaves a claim that is renewed forever
  and reads `running` until restart.

The issue's own test (`TestHungReconcilesDoNotStarveOtherResources`, posted
in the triage) fails at `e6f5ea446`: four reconciles that block until their
context ends keep a fifth from ever running.

## Decision

### 1. The server bounds each archive and remove call to a pool agent

`poolruntime.Provider.Archive` and `Provider.Remove` run their call to the
agent under a deadline of their own: **5 minutes**, a constant in
`server/providers/poolruntime` (`poolAgentTeardownDeadline`), not
configuration. The bound is on the provider's call, not on the reconcile:
resolving the provider, opening secret state and writing the row are not
under it.

5 minutes is longer than a healthy teardown's answer: the force-remove of a
container, the removal of a tree, and an archive whose working-root stash
fits in it. A stash that does not fit keeps going on the agent past the
server's deadline (it already runs under `context.WithoutCancel`,
`pool-agent/sandboxruntime/workingroot.go`), and a retry finds it done.

### 2. An expired teardown deadline is the pool not answering

The deadline is armed with `context.WithTimeoutCause` and a sentinel cause,
as the create's recovery ceiling already is (`errPoolWaitExpired`,
`provider.go`). When the call fails and the cause is that sentinel, the
provider returns `sandbox.ErrPoolNotReachable`, wrapping an error that
names the pool, the operation and the bound:
`pool <id> did not answer the archive of sandbox <id> within 5m0s`.
`teardownNotAnswered` then holds as written: the generation stays
unobserved, an archive or repair stays `archiving`/`repairing` (a delete
never settles anyway), the engine retries with backoff, and the job's
`last_error` says why. Nothing about the
deadline settles a teardown as failed.

`poolAgentUnanswered` is unchanged. A timeout with any other cause is still
not "the agent went away", which keeps the create's reading of it.

### 3. The pool agent's teardowns wait for the power lock only while their request lives

The per-sandbox power lock becomes a lock whose acquire can be abandoned
(a one-slot channel, or equivalent) with a context-taking acquire beside the
plain one. `ArchiveSandbox` and `DeleteSandbox` take it with the request's
context, in both the Docker and machine runtimes. When the server's deadline
ends the request, the teardown stops waiting and does nothing. Without this,
each retry of a teardown the server gave up on would leave one more waiter
queued on the agent. Each would run, unasked, when the lock frees, writing
an archive marker for a request nobody is waiting on.

The other holders of the lock (start, stop, restart, suspend, power-down,
and the harness, tree, runtime-config and cache-clear paths) keep the plain
acquire. Their calls are not bounded by this ADR (Deferred).

### 4. A claim re-checks `not_before`

`claimOne`'s guarded `UPDATE` adds `not_before <= now`. A row armed for the
future between the candidate read and the claim is skipped, as a row
another node claimed is. `reconcile/DESIGN.md` "Claiming and leases"
changes its step 2 to match.

### 5. A worker renews only the claims it has in flight

The engine keeps the set of rows it is executing (resource type and id),
which it already counts per type in `running`. `leaseLoop` renews
`claimed_by = me` only for those rows. A claim whose settle write failed is
no longer renewed. It lapses at its lease and the row is claimable again,
which is the level-triggered recovery the dirty set already relies on for a
dead node. `complete` and `release` log a settle write that fails, so the
lapse has a line to explain it.

## Alternatives rejected

- **(b) An engine-wide cap on how long one reconcile may run.** One number
  cannot fit every reconciler. A pool's provisioning legitimately runs to
  `provisionTimeout` (15 minutes, `poolruntime/box.go`). A sandbox create
  waits out pool capacity and an agent that is coming back
  (`poolCapacityWaitTimeout`, `poolAgentWaitCeiling`). A cap long enough for
  those leaves a hung teardown holding a slot for as long, and a cap short
  enough to help cancels healthy work at whatever point it had reached. It
  also hides which call hung: the reconcile ends with `context deadline
  exceeded`, not "pool P did not answer the archive". The bound belongs on
  the call that waits on another party, where its expiry can be given a
  meaning (§2).
- **(c) A separate concurrency lane for teardowns.** Only moves the
  stall. Hung teardowns would still starve every other teardown, so `rm`
  would still sit in `archiving` for hours. A hung start would starve
  creates the same way. Lanes also need a lane key on the dirty row, which
  is a schema change, to split one resource type's slots by what its next
  reconcile will do, which the engine cannot know before it runs it. See
  Deferred for when to revisit.
- **Teardown becomes an instruction, answered on acceptance.** It would free
  the slot at once. ADR 0022 §3 decided against it for a reason that still
  holds: acceptance of a destructive act on state only the agent can see is
  a promise the control plane cannot verify, and for a delete the row it
  would verify against is the thing being removed. Superseding that is out of
  proportion to this bug.
- **Counting every client-side timeout as unanswered** (changing
  `poolAgentUnanswered`). A create that times out may still be running on
  the agent, which is why the function excludes timeouts. Only the
  teardown's own deadline, identified by its cause, is given the meaning.
- **A shorter deadline (under a minute).** It would bound the worst-case
  stall of a create more tightly, but a healthy teardown on a loaded pool
  (a container with a large writable layer, a big tree) would then miss it
  routinely. Its retries would wait out the lock behind the abandoned call
  (§3) and could keep missing it.

## Consequences

- A teardown the pool never answers holds a `sandbox` slot for at most 5
  minutes per attempt, then backs off (up to `DefaultBackoffMax`, 5 minutes).
  With every slot taken by such teardowns, a new box's create waits at
  most about one deadline for a slot, not until the server restarts.
- A box being archived on a pool that doesn't answer stays `archiving`, and
  `admin job ls` shows its row cycling with a `last_error` naming the pool
  and the call. The archive finishes on the first attempt the pool answers.
- The reporter's `running` rows with a 24-hour NEXT are gone (§4, §5).
- What held the lock on the reporter's pool is not identified here. The
  server log around the archives would show it, as a start or stop that
  never returned. It is a separate bug, filed when it is seen.

## Deferred

- **Bounding the power instructions** (start, stop, restart, suspend,
  power-down) and making their lock waits abandonable. They take the same
  lock and can hang the same way. But the agent stops a start's work when
  its request ends, so a deadline shorter than a slow healthy start (a VM
  boot, a pool waiting for its Docker daemon) would make that start never
  succeed, and that needs its own sizing. Revisit when a start or stop
  reconcile is seen holding a slot past what a teardown is now allowed.
- **Bounding the dispatched create.** Once the agent has taken a create, it
  runs for as long as the create takes (the create's own budgets cover only
  waiting for capacity and for an agent coming back). Revisit when a create
  is seen holding a slot with its agent present.
- **Separate lanes (c).** Revisit if, after this, creates are seen waiting
  minutes behind teardowns that are cycling through their deadlines.
- **Showing why a box waits.** The issue asks that `ls`/`run` say why a box
  is still `starting` when its reconcile has not been claimed. This ADR makes
  that wait short. Saying so on the box is a separate change.
- **`wait`/`capture` answering 404 for a `starting` box, and the expired
  harness login.** Separate symptoms in the same report, filed on their own if
  they recur after this lands.

## Open, for implementation

- Whether the engine test from the triage stays in `server/internal/reconcile`
  (it pins the outcome) or moves next to
  `server/internal/resources/sandboxes/archive_unreachable_pool_test.go`
  with an `Archive` that blocks until its context ends. The fix is in the
  provider, so the sandboxes package is the likelier home. The engine fixes
  (§4, §5) get their own tests in `reconcile`.
- That a cancelled server request actually ends the agent handler's context
  across the pool's transport (the lease's tunnel), so §3's wait is
  abandoned. If it does not, that is fixed as part of this.
- The lock's type and the name of its context-taking acquire.
