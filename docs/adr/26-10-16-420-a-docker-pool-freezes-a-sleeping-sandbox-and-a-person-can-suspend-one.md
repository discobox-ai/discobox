# 26-10-16-420 — A Docker pool freezes a sleeping sandbox, and a person can suspend one

- **Status**: Accepted
- **Date**: 2026-10-16
- **Amends**: [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md)
  §5, where every runtime implemented power-down for sleep as a stop, and its
  Deferred **Freeze**, which this decides for the Docker runtime.
- **Relates to**: [ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §§9–10, power instructions and the observed runtime state;
  [ADR 26-10-12-711](26-10-12-711-using-a-failed-discobox-with-no-container-repairs-it-once.md)
  §2, the start every use makes;
  [ADR 0034](0034-sandbox-state-and-runtime-state-are-separate-fields.md), the
  two state axes.

## Context

26-10-16-337 lets an agent sleep and name its wake, and lets the runtime
choose whether a sleep freezes or stops. Every runtime stopped, because none
could freeze. Every pool runs its sandboxes as containers on a Docker daemon,
and Docker can freeze one: `docker pause` puts its cgroup in the freezer, and
`docker unpause` thaws it. Memory, `/run`, and every process — the harness
mid-session above all — survive, and nothing boots on the way back.

A person wants the same thing on demand: park a box that holds something warm
without losing it, from `discobox admin`, and have it come back the way a
stopped box does, by being used.

## Decision

**Suspended is a runtime state. The Docker runtime freezes a confirmed sleep
with `docker pause`, a person can suspend a running sandbox with a new power
instruction, and every start — explicit, scheduled, or on use — thaws a
suspended sandbox rather than booting it.**

### 1. `suspended` is an observed runtime state

A suspended sandbox has its container and its memory, and nothing in it runs.
The pool agent reports it (`runtimeState: suspended`, and so `displayState`),
from Docker's `Paused` and its `pause` and `unpause` events. It is not
`running` — nothing inside it answers — and not `stopped`: its container is
live and must not be rebuilt under it (`SandboxIsLive`).

### 2. Sleep freezes on the Docker runtime

A confirmed power-down for **sleep** pauses the container. A pause that fails
— a daemon without a cgroup freezer — falls back to a stop: the agent asked to
go down, and a stop is the power-down every runtime can do. **Idle** still
stops, for 26-10-16-337's reason.

### 3. Suspend is a power instruction, and it fails where it cannot freeze

`POST /projects/{p}/sandboxes/{s}/suspend`, carried to the pool's new
`suspend` route, beside start, stop and restart: `Suspend` is a required method
of the provider contract and of the pool runtime. It is a person's instruction
from outside, so it cancels the sandbox's scheduled wake as a stop does
(26-10-16-337 §6) — once the pool has frozen it, since a refused suspend
changes nothing. Unlike sleep it never falls back: a person who asked to
suspend and got a stop has lost what they meant to keep. A pool agent from
before the route, or a pause that fails, is an error that changes nothing. A
sandbox that is not running is refused; one already suspended is a no-op.

The CLI offers it as `discobox admin box suspend` only, and the sandbox role
does not get it: a lead parks its workers by stopping them.

### 4. Every start thaws a suspended sandbox

Start, the scheduled wake's start, and the start every use makes
(`EnsureSandboxRunning`) unpause a suspended sandbox under the same power lock
and start gate. Restart and stop act on it as on a running one: Docker
delivers the stop signal once the container thaws. Archive, delete and a cache
clear do too.

After a thaw the pool delivers the sandbox's runtime-config document — a
secret recorded while it was frozen reaches it — and tells the sandbox agent
it has resumed (`POST .../autostop/resumed`, pool-only, the confirm route's
scope). The agent moves the wakes now due into `~/.discobox/wake/.done/`, as a
boot does. Left in place, the wake that brought a frozen sandbox back would be
returned by its next power-down's confirmation, and its next sleep would be
undone at once. Both are done under the power lock, on a context the caller
going away cannot cancel. The thaw is also a start for the idle clock: the
wall clock ran on through the freeze, and a sandbox frozen past its idle
timeout would otherwise be stopped the moment it thawed.

### 5. A frozen sandbox is read like a stopped one

Nothing inside it answers, so the pool polls no status from it, delivers it no
document, refuses the reads that never start a sandbox (`requireRunning`) as
`suspended`, and refuses to export its tree. An attached client whose sandbox
is suspended is detached as from a stopped one; its next attach resumes it.

## Alternatives rejected

**A suspended sandbox reports `running`.** Docker's own `Running` is true for a
paused container. Rejected: everything that reads `running` — the status poll,
the use path's shortcut, an attach — would reach into a sandbox that cannot
answer and hang until it times out.

**Resume is its own instruction.** Rejected: a stopped sandbox comes back by
being used or started, and a suspended one is the same to the person using
it. A second verb would also be one every on-use start had to know about.

**Suspend falls back to a stop, as sleep does.** Rejected: the person asked to
keep the box's memory. Telling them it cannot is better than discarding it.

**The thawed harness is told it was woken.** `DISCOBOX_WAKE` reaches a
harness at launch, and a frozen one is never relaunched. Rejected for the
reason 26-10-16-337 rejected typing a wake into a terminal; see Deferred.

## Consequences

- An agent's sleep on any Docker pool keeps its harness, its terminals and
  everything in `/run`; on waking it continues where it froze.
- A suspended sandbox holds its memory on the host until it is resumed or
  stopped. Nothing stops one on its own.
- The API's `runtimeState` and `displayState`, and the pool's state report,
  gain `suspended`; the server API gains `suspend-sandbox`; the pool API gains
  `pool-suspend-sandbox`; the sandbox agent gains `resume-sandbox-autostop`.
  The pool agent ships with the control plane that accepts its reports.
- A frozen sandbox's clocks jump on resume. Connections it held may be gone.

## Deferred

- **Telling a thawed harness it was woken.** Revisit with 26-10-16-337's
  Deferred **Waking a running harness**, which is the same problem.
- **Freezing an idle sandbox whose agent is blocked on a person**
  (26-10-16-337, Deferred) has the freeze it waited for on Docker pools.
  Revisit when a user loses a blocked prompt to an idle stop.
- **A lead suspending its workers.** Revisit when an orchestrator needs it.
