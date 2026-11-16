# 26-11-16-372 — A sandbox powers down when its primary harness ends

- **Status**: Proposed
- **Date**: 2026-11-16
- **Amends**: [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md)
  §4 (the status gains `primaryExitedAt`), §5 (power-down gains the reason
  **exit**, ahead of sleep and idle), and §8 (the field travels only under
  control-plane power).
- **Narrows**: [26-11-04-746](26-11-04-746-a-sleep-request-has-a-fifteen-second-grace.md)
  §1. A pending sleep's grace and working deferral do not hold back a
  power-down for exit.
- **Relates to**: [0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md),
  the idle policy this sits beside;
  [0027](0027-harness-terminals-run-as-a-shells-typed-in-job.md), under which
  the harness is a shell's typed-in job;
  [0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md),
  revive in place;
  [26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md),
  sleep as a freeze on Docker; issue #1235.

## Context

The owner asked that a sandbox shut down when its primary terminal exits,
for any reason, through the control plane's power path, on every runtime.
Today it does not, on any runtime. Neither the runtime nor boxd causes this.
It follows from two decisions:

- **The harness is not the exec.** Under ADR 0027 the primary terminal's
  process is a login shell, and the harness is a command typed into it
  (`execs.CreateRequest.StartupCommand`, written by
  `sandbox-agent/execs/shim.go` `writeStartupCommand`). Quitting Claude Code
  ends the harness's job and leaves `bash -l` at a prompt. The exec keeps
  running, and the shim's screen says that is the normal case
  (`sandbox-agent/shimruntime/screen.go`, `programStatusNow`).
- **Only inactivity stops a sandbox.** `autostop.Policy.Evaluate`
  (`sandbox-agent/autostop/autostop.go`) reads screen changes, attachers,
  program status, holds and leases, and the sleep directory. Nothing in it
  looks at whether the primary's harness is still there, so a quit harness
  waits out the idle timeout (30 minutes by default). Even an exec that does
  exit (the shell harness, or `exit` typed after quitting) only stops being a
  source of activity.

The control plane's path for a stop the sandbox asks for already exists
(26-10-16-337 §5). The status reports a reason. The server marks the
sandbox's power reconcile (`power_reconcile.go`, `powerDownAsked`). The
runtime, under its per-sandbox power lock, has the sandbox confirm that reason
fresh, then powers it down. Today the reasons are `idle` and `sleep`
(`server/internal/sandbox/runtime.go`, `PowerDownReason`). A runtime freezes
only for `sleep` and stops for anything else
(`pool-agent/sandboxruntime/power.go`, `PowerDown`). The idle path already
stops a boxd machine, not just the guest.

`discobox-power` has no off verb. Its `sleep` writes a request file that
becomes the `sleep` reason, which freezes on Docker (26-10-16-420). "The
same call `discobox-power` uses" is therefore read as this path, with a
reason that always stops.

Configure mode, which ADR 0108 exempted from the policy, no longer exists
([26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md)),
so it needs no exception here.

## Decision

**When the primary terminal's harness has started and then ended, or the
primary exec itself has exited, the sandbox agent reports it on its status.
The control plane powers the sandbox down for a new reason, `exit`: always a
stop, ahead of sleep and idle, confirmed fresh under the power lock like the
others. Nothing in the sandbox holds it up: not clients, leases, services,
forwards or a pending sleep.**

### 1. The primary has exited when its harness job ends, or its exec does

The primary terminal (`terminal.IsPrimary`) has **exited** at the first
moment either of these is true:

- **Its harness job has ended.** The harness job is the process group the
  harness runs in: the typed-in job of ADR 0027. It has ended when that
  group has no processes left. A suspended job (Ctrl-Z) still has its
  processes, stopped, so a suspend is not an exit. A harness that re-execs
  itself, or a launcher that restarts its own child (Codex's retry), stays
  in the group and is not an exit either.
- **Its exec has exited.** This covers the shell harness (the shell is the
  terminal and no job is typed in), `exit` typed after quitting the harness,
  and a shell that dies.

A harness job counts only once the harness itself has started. Before that,
the primary is not running a harness, and its ending is not an exit:

- **A failed pre-harness hook is not an exit** (owner, 2026-11-16).
  `discobox-primary-run` (`sandbox-agent/cmd/discobox-sandbox-agent/primary.go`)
  runs `.discobox/scripts/pre-harness` and, when the hook fails, returns to the
  login shell for recovery without starting the harness. That shell stays up
  and the sandbox idles out on the pool's timeout as before. A hook that
  passes hands over to the harness by `exec`, in the same group, so the
  harness job is the runner's job from that point on.
- **The job announces itself when the harness starts.** The shim does not
  infer the harness job by sampling the PTY's foreground group, because a
  harness that exits between two samples would never be seen. The harness job
  tells the shim its group at the moment the harness starts, and the shim
  records it. The runner is what does this, immediately before it `exec`s the
  harness, so a failed hook announces nothing. Every harness primary is
  therefore launched through the runner, with or without a pre-harness hook.

The shim keeps the record (the announced group, and when it ended) and
reports it with the exec's status, as it reports `titleChangedAt`. The shim
outlives a sandbox agent restart, so the record does too.

A new run of the primary gives a new harness job. A revive in place (ADR 0038)
is one: an attach to an exited primary, or the next boot. Its announcement
replaces the record, and the primary has not exited again until that job
ends. A harness started by hand in the primary's shell after the harness has
quit does not announce itself. It does not cancel the exit.

### 2. The status reports it, and the control plane powers down for `exit`

`SandboxAgentAutostopStatus` gains `primaryExitedAt`, the time the primary
exited (§1). It is present only while the primary's latest run has exited
and the control plane holds the sandbox's power, and is reported beside
`idle`, `sleep` and `wakeAt`.

The power reconcile reads it as a third power-down reason, **exit**
(`PowerDownExit` beside `PowerDownIdle` and `PowerDownSleep`, in
`server/internal/sandbox`, `pool-agent/sandboxruntime` and the confirm
route's `reason` enum). It ranks ahead of both:

- **Over sleep.** A runtime freezes for sleep to keep what is in memory. Once
  the harness has ended, there is nothing in memory worth keeping. A pending
  sleep request does not get its grace or working deferral (26-11-04-746)
  either: the program that would be writing a closing response is gone.
- **Over idle.** Idle's confirmation is refused while a client is connected
  (26-10-16-337 §5). The owner asked for a stop "for any reason", and an
  attached console is the commonest thing that would otherwise hold the box
  up.

Every runtime **stops** for exit: docker, libkrun, vz, hcs and boxd. On boxd
the stop is the machine's, as an idle stop already is. Every runtime already
stops for any reason that is not sleep, so no runtime's power-down method
needs a new branch.

### 3. Exit is confirmed fresh, and nothing in the sandbox refuses it

Under the per-sandbox power lock, the runtime calls the confirm route with
reason `exit`. The sandbox confirms it only if the primary's latest run has
exited **now** (§1). Two things refuse it:

- **A revive in the window.** An attach that revived the primary since the
  report started a new harness job, so the sandbox is no longer exited and
  stays up.
- **A record that went stale.** A status stored from before the sandbox last
  stopped is refused, as for idle and sleep.

Nothing else is consulted. Attached clients, keepalive leases, declared
services, forwards, other terminals and background jobs (including the
primary shell's own) do not hold the sandbox. An attached client is
disconnected, as by any stop.

As for idle, the confirmation answers with the sandbox's earliest pending
wake, and the server schedules it (26-10-16-337 §§5–6). An exit is the
sandbox's own power-down, not an instruction from outside, so it does not
cancel the agent's wake. A box whose agent scheduled a wake and then quit is
started at that time, and its relaunched harness is handed the wake.
Confirming exit does not consume sleep requests. They live in `/run`, and the
stop clears them.

### 4. Without control-plane power, the sandbox powers itself off

An agent whose pool has not handed its power to the control plane
(26-10-16-337 §8) acts on exit the way it acts on idle there: `Policy.Run`
powers the sandbox off itself at its next tick. It reports `primaryExitedAt`
only once it holds `controlPlanePower`. The status object refuses fields it
does not know (`additionalProperties: false`), so servers must accept the
field before agents send it. The server and pool ship together. An agent
older than this change never reports the field, so it is never asked to
confirm `exit`.

### 5. The `discobox` skill says so

The image's `discobox` skill, which every harness reads, says that quitting
or crashing the harness, or exiting the primary terminal, stops the box
within about half a minute, whatever else is attached or held. It also says
that the next attach starts the box again and resumes the harness.

## Alternatives rejected

**The primary exec exiting is the only trigger.** This needs nothing from
the shim, but it never fires in the owner's own case. Quitting Claude Code
leaves the shell running (ADR 0027).

**Make the harness the exec's own process again**, or type `exec <harness>`
so it replaces the shell. Either way the harness becomes the session leader,
in an orphaned process group, and the kernel discards Ctrl-Z. ADR 0027
exists to avoid exactly this.

**Type `<harness>; exit`, so the shell ends with the harness.** The trigger
would then be the exec exiting, with no new shim record. It was rejected
because suspend behaviour depends on the shell. On Ctrl-Z, bash runs the
next command in the list, so the shell would try to exit with a stopped job.
The recovery shell after a failed hook also needs the list to branch on the
runner's status. The announced group says what happened directly, on every
shell.

**Infer the harness job by sampling the PTY's foreground group.** No
cooperation is needed from the job, but a harness that starts and exits
inside one sample interval is missed. A sample also cannot tell the harness
from a failed pre-harness runner, which the owner ruled is not an exit (§1).

**Report it as `idle`, with the cause in `lastActivity`.** No new reason
would be needed. It was rejected because idle's confirmation is refused while
a client is connected, and the owner asked for a stop regardless. Clients,
the runtime and the logs would also read one reason for two different claims.

**Report it as `sleep`.** On Docker that freezes the sandbox, keeping in
memory a box whose harness has gone. It also waits out sleep's grace.

**The agent powers itself off even under control-plane power.** That is
immediate, but on a boxd machine powering the guest off from inside does not
stop the machine (26-10-16-337, Context). It would also bypass the power
lock that serializes a stop with an attach's start.

**A grace period after the exit, so a person who quits the harness can keep
using the shell.** The owner asked for "for any reason", and an attached
person is the case a grace would protect. It is listed under Deferred.

## Consequences

- Quitting or crashing a harness stops its box about one status poll (15 s),
  a push and a reconcile later. Before this change the box waited out the
  idle timeout (30 minutes by default).
- A person who quits the harness to use the primary's shell loses the box
  shortly after. Their next attach starts it and relaunches the harness, and
  another terminal opened on the box does not keep it up.
- Background work that is not in the harness's process group does not keep
  the box up once the harness ends: a build started with `&`, a declared
  service, a held lease. A child left in the harness's own group does keep
  it up until that child ends.
- A failed pre-harness hook leaves the box up for recovery, as today (§1).
- A wake the agent scheduled survives its harness quitting (§3).
- Every harness primary on Linux now launches through `discobox-primary-run`,
  not only one with a pre-harness hook.
- The API contract gains `primaryExitedAt` on `SandboxAgentAutostopStatus`
  and `exit` in the confirm request's `reason` enum. The provider contract's
  `PowerDownReason` and the pool agent's gain `exit`. Neither gains a new
  method.

## Open (settled in implementation)

- **The announcement's channel**: a private sequence on the PTY, which the
  shim's screen already parses for OSC 7501, or a file in the exec's runtime
  directory. Either must arrive before the harness can write anything, and a
  program in the box that forges it can only power down its own box, which
  `discobox-power sleep` already allows.
- **Non-Linux primaries** (macOS guests on vz): the runner fronts the primary
  on Linux only today (`terminal.Service.primaryStartupCommand`). Whether it
  fronts it there too, or that platform counts only the exec's exit, is
  decided with that platform's runner.

## Deferred

- **Waking the pool at once on exit**, as a port change does
  ([26-11-03-815](26-11-03-815-a-pool-waits-on-a-sandboxs-ports-and-reports-a-change-at-once.md),
  [26-11-09-062](26-11-09-062-a-harness-change-wakes-the-pool-like-a-port.md)),
  instead of at its next 15 s poll. Revisit if the poll's latency shows up as
  a cost or a complaint.
- **A grace after the exit for an attached person.** Revisit if people quit
  the harness to work in the primary's shell and lose the box to it.
- **Terminals other than the primary.** A harness in a second terminal ending
  stops nothing. Revisit if a box is ever driven by a harness that is not its
  primary.
