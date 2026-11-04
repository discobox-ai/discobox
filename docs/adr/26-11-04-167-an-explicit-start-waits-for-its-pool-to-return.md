# 26-11-04-167 — An explicit start waits for its pool to return

- **Status**: Proposed (would supersede [26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md) §6 only for an explicit start refused with `ErrPoolNotReachable`; narrows [26-11-03-618](26-11-03-618-a-discobox-owed-its-delivery-is-never-started-and-attach-repairs-it-to-deliver.md) §2's statement that a start instruction waits for nothing, without changing its source-delivery refusal)
- **Date**: 2026-11-04
- **Issue**: [#569](https://github.com/discobox-ai/discobox/issues/569), following [#544](https://github.com/discobox-ai/discobox/issues/544) and [#567](https://github.com/discobox-ai/discobox/pull/567)
- **Preserves**: [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) §9's power instructions, and [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md) §6's single-send scheduled wake and cancellation by outside instructions.

## Context

A server upgrade replaces pool agents. A person starting a stopped box during
that replacement currently gets a 409 saying the pool's agent is not answering
and to try again shortly. The person has already asked for the action that
would succeed once the replacement finishes. The owner chose on #569 to wait,
show that the start is waiting for the pool to come online, and time out after
about five minutes.

Checked against `main` at `b70abf7f`:

- `server/internal/resources/sandboxes/service.go` routes explicit start and
  restart through `instructSandbox` and `awaitUsable(waitOnceRebuilding)`.
  `runtimeRefusal` preserves `sandbox.ErrPoolNotReachable` as the cause of
  the 409. A start currently waits only after finding no container.
- `server/internal/resources/sandboxes/attach_wait.go` waits for actions from
  their first attempt. Its two-minute stall budget renews on provisioning
  progress; a separate five-minute pool-host ceiling bounds a wait whose
  attempts themselves cause pool reconciliation progress.
- `instructSandboxOnce` cancels a scheduled wake on every attempt. Reusing
  that whole operation as a retry would repeatedly move the wake epoch,
  including after a newer stop had already canceled the waiting start.
- `server/internal/resources/sandboxes/power_reconcile.go` sends a due wake
  directly through the single-attempt `instructSandbox` function in
  `power.go`. Its reconcile slots must not wait for an absent pool host.
- `cli/internal/cli/lifecycle.go` clears the delivery status before posting
  the ordinary start. `server/internal/handlers/sandboxes.go` answers the
  start with a single JSON response. Neither currently communicates a
  request's pool wait while that request is pending.

## Decision

### 1. Explicit start waits only for the additional pool refusal

The server retries an explicit start when its attempt returns an error for
which `errors.Is(err, sandbox.ErrPoolNotReachable)` is true. This covers
`StartSandbox`, whether called by the ordinary CLI, administrative CLI, console
or an authorized sandbox client. A typed repair's final start uses the same
policy, after its rebuild has converged and only if no source delivery is owed.
The policy belongs to this request path, above the provider.

The additional wait does not turn start into an action acquire that waits for
every provisioning refusal. Missing runtime before initial placement, ordinary
initial provisioning refusals, authorization failures, unsupported operations,
and source owed to the box remain immediate answers. Existing automatic repair
and waiting after finding no container remain as 26-10-12-711 specifies.
Recovering pool connectivity alone does not authorize waiting on those other
refusals. Each attempt re-reads and validates the current row.

Restart gains no pool-unreachable retry: replaying its stop/start sequence can
interrupt a box twice when the first answer was lost. Its existing no-container
repair behavior remains. Stop and suspend gain no wait. The power reconciler's
scheduled wake remains single-send, including when the pool is unreachable.
An explicit start remains an instruction with a request lifetime; it records
no desired running state, lifecycle generation or background start job.

### 2. One bounded request, under the existing wait budgets

Use the attach wait's progress-sensitive two-minute stall budget, the caller's
context, and its five-minute pool-host ceiling. The first pool-unreachable
refusal starts that ceiling. Repeated transport failures, pool reconcile
writes and renewed progress cannot extend it. For this explicit start, keep
that deadline for the rest of the request, including a transition into the
existing missing-container repair wait; an intermittent answer must not grant
another five minutes. Whichever bound expires first ends the wait. Five minutes
is a maximum, not a minimum: a stalled pool can fail sooner.

No new provider retry loop is added. The service releases any acquired client
between attempts and waits at the existing poll interval. Attempts themselves
must observe the remaining budget, so a hung RPC cannot outlive the ceiling.
Cancellation stops retries promptly; it does not undo a start already delivered.
A server restart ends the request and never resumes it in the background.

On a wait deadline, return the last relevant refusal with its pool context and
409 classification, explaining that waiting ended. Do not replace it with a
bare timeout, report success, or claim that an unanswered instruction could
not have run. Cancellation remains cancellation. A successful pool start uses
the existing success contract; runtime reports still own the observed state.

### 3. A later instruction supersedes the waiting start

Admit an explicit start once and cancel its scheduled wake once, retaining the
resulting `WakeEpoch` as the request's ordering token. Retries never cancel a
wake again or obtain a new token. Before every resend, require that token to
remain current and that the sandbox still exists, is present, and is not owed
source. A newer outside start, stop or restart supersedes the waiting start
even if that newer instruction fails to reach the pool. Suspend retains
[26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md)
§3: it moves the wake epoch and supersedes this wait only after successful
freeze; a refused suspend changes neither the wake nor the pending start.
Serialize that successful completion with retry dispatch as well.
Repair admission also supersedes an older waiting start; its own final start
retains the repair request's ordering token across the rebuild, so it cannot
start after a stop admitted during that rebuild. Archive and delete end the
wait too; unarchiving later must not revive the old request. Return a conflict
explaining supersession, rather than silently succeeding or retrying.

The token check and dispatch must be serialized with admission of newer power
instructions and archive/delete for that sandbox. A check followed by an
unguarded send is insufficient. Release serialization while waiting between
attempts so stop can proceed immediately during the pool outage. An attempt
already dispatched is ordered before a later admitted stop; the pool's existing
per-sandbox power serialization still orders execution. Transport ambiguity
must not permit that old attempt or a queued resend to execute after the later
instruction: implementation must fence stale dispatches at the execution
boundary if server serialization cannot establish that ordering. Do not hold
a database transaction over network I/O.

This extends the use of wake cancellation bookkeeping, not the desired-state
model. Any added durable fencing metadata must safely migrate existing rows
and preserve the store's ownership of fields across whole-row updates.

### 4. The wait is visible while it happens

While an explicit start is pending on its pool, the CLI's status and the
console's busy status say **“waiting for the pool to come online”**, naming
the affected box/pool. Show it when the server enters that wait, keep it while
waiting, and clear or replace it on success, another phase, supersession,
cancellation or failure. Non-interactive output emits a bounded status message
on stderr; JSON result output remains parseable. Repair narrates this phase
in its existing status too.

This is request progress, not the sandbox's observed runtime state or a
provisioning heartbeat. It must not renew the stall budget or invent a
`running` observation. A generic spinner or a message printed only after the
409 fails the owner's requirement. The server must expose enough request
progress for clients to distinguish this wait while the call is pending.

The progress transport and exact serialization/fencing primitives are open
implementation choices. They must preserve the existing JSON start contract
for clients that do not consume progress and demonstrate the ordering above;
choosing `waitFromTheStart` alone or polling stale runtime state is insufficient.
No additional product decision is deferred.

## Alternatives rejected

- **Return the 409 and ask the person to retry.** This is understandable but
  leaves the first command after an upgrade failing for an expected transition;
  the owner explicitly chose to wait and narrate it.
- **Retry inside the provider.** That also replays scheduled wakes, which can
  arrive after a later stop, and holds scarce power reconcile slots while a
  host is absent. The provider cannot distinguish these request policies.
- **Use the action wait unchanged.** It also broadens start to initial
  provisioning and missing-runtime waits. It neither supplies request status
  nor makes repeated wake cancellation safe.
- **Keep retrying while progress changes.** The attempts can cause the progress
  themselves; an unreachable pool would then wait indefinitely.
- **Hold the sandbox's ordering lock for the whole wait.** A later stop would
  wait behind a five-minute start, rather than superseding it.
- **Make start durable desired state.** A background retry could run after
  the caller leaves or a later stop. This reverses 0017 §9 for a temporary
  pool-agent replacement.

## Consequences and acceptance evidence

Implementation must cover recovery after one or several pool-unreachable
answers, the status while pending, stall/ceiling/caller deadlines, cancellation,
and a later stop, successful suspend, restart, repair, archive or delete
superseding the wait. A refused suspend must leave the pending start and its
wake bookkeeping unchanged. Include a race at the check/dispatch boundary and an unanswered first
attempt, not only a stop between two polls. Verify that retries cancel the wake
once, initial provisioning and owed source still refuse, typed repair's final
start follows this policy, and scheduled wakes/restart gain no new replay.

The issue's runtime acceptance remains a real v0.18.0 → next upgrade: the first
`discobox start <stopped box>` after install visibly waits if its pool agent is
being replaced, exits zero without a manual retry, and the box is observed
running. This proposal supplies the decision; it does not implement or claim
that validation.
