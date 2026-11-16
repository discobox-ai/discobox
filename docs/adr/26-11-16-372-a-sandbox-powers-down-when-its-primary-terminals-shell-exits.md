# 26-11-16-372 — A sandbox powers down when its primary terminal's shell exits

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
through the control plane's power path, on every runtime. Today none does.

Under ADR 0027 the primary terminal's process is a login shell, and the
harness is a command typed into it (`execs.CreateRequest.StartupCommand`,
written by `sandbox-agent/execs/shim.go` `writeStartupCommand`). Quitting the
harness leaves the person at that shell's prompt. Exiting the shell ends the
exec: the shim reaps it and records it `exited`, and the attached client's
stream ends. A client's done-check reads the primary without reviving it
(`sandbox-agent/server/handler.go`, `resolveExecIDReadOnly`), so a client that
sees the exit does not bring the primary back by looking.

Nothing then powers the sandbox down. `autostop.Policy.Evaluate`
(`sandbox-agent/autostop/autostop.go`) reads only activity (screen changes,
attachers, program status, holds and leases) and the sleep directory. An
exited exec merely stops being a source of activity, so the sandbox waits
out the idle timeout (30 minutes by default) after the person has left.

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

## Decision

**When the primary terminal's shell exits, the sandbox agent reports it on its
status. The control plane powers the sandbox down for a new reason, `exit`:
always a stop, ahead of sleep and idle, confirmed fresh under the power lock
like the others. Quitting the harness is not an exit. The person is back at
the shell, and the sandbox stays up until they leave it.**

### 1. The primary has exited when its exec's process ends

The primary terminal (`terminal.IsPrimary`) has **exited** when the process of
its latest run (the login shell, `bash -l` or whichever shell
`execs.ResolveShell` chose) has ended, whatever its exit code or signal. These
are exits:

- `exit` or Ctrl-D typed at the shell, after the harness was quit or with no
  harness ever started;
- the shell harness's shell ending, since that shell is the terminal;
- the shell being killed or dying.

These are not:

- **The harness ending.** Quitting Claude Code, `discobox-harness-run`
  returning, or the harness crashing leaves its shell running and at a prompt
  (ADR 0027), so nothing happens. A person may run the harness again, work in
  the shell, or leave it. A sandbox left at that prompt idles out on the
  pool's timeout as it does today.
- **A failed pre-harness hook.** `discobox-primary-run` returns to the shell
  for recovery, so the shell is still running.
- **A launch that never ran a shell**, such as a primary whose start failed
  (`markStartFailed`) or whose record outlived its process across a boot
  (`lost`). That is a broken launch, not a person leaving. Powering down for
  it would repeat on every start.

The shim already records the end of an exec's process (`ExitedAt`, status
`exited` or `failed`) at the moment it reaps it. That record is the trigger.
How the policy tells a shell that ran and ended apart from a launch that never
ran one is left to implementation, from what the shim already records. The
shim outlives a sandbox agent restart, so the record does too.

A new run of the primary is not exited. A revive in place (ADR 0038), whether
by an attach to the exited primary or by the next boot, starts a new process
under the same exec id. The primary has not exited again until that process
ends.

### 2. The status reports it, and the control plane powers down for `exit`

`SandboxAgentAutostopStatus` gains `primaryExitedAt`, the time the primary's
latest run ended (§1). It is present only while that run has exited and the
control plane holds the sandbox's power, and is reported beside `idle`,
`sleep` and `wakeAt`.

The power reconcile reads it as a third power-down reason, **exit**
(`PowerDownExit` beside `PowerDownIdle` and `PowerDownSleep`, in
`server/internal/sandbox`, `pool-agent/sandboxruntime` and the confirm
route's `reason` enum). It ranks ahead of both:

- **Over sleep.** A runtime freezes for sleep to keep what is in memory. The
  terminal a person was working in has gone, so there is nothing in memory
  worth keeping. A pending sleep request does not get its grace or working
  deferral (26-11-04-746) either.
- **Over idle.** Idle's confirmation is refused while a client is connected
  (26-10-16-337 §5). The person has left the primary, and another client
  attached elsewhere does not hold the box up (§3).

Every runtime **stops** for exit: docker, libkrun, vz, hcs and boxd. On boxd
the stop is the machine's, as an idle stop already is. Every runtime already
stops for any reason that is not sleep, so no runtime's power-down method
needs a new branch.

### 3. Exit is confirmed fresh, and nothing else in the sandbox refuses it

Under the per-sandbox power lock, the runtime calls the confirm route with
reason `exit`. The sandbox confirms it only if the primary's latest run has
exited **now**. Two things refuse it:

- **A revive in the window.** An attach that revived the primary since the
  report started a new run, so the sandbox is no longer exited and stays up.
- **A record that went stale.** A status stored from before the sandbox last
  stopped is refused, as for idle and sleep.

Nothing else is consulted. Clients attached to other terminals, keepalive
leases, declared services, forwards and background jobs do not hold the
sandbox. Any client still attached is disconnected, as by any stop.

As for idle, the confirmation answers with the sandbox's earliest pending
wake, and the server schedules it (26-10-16-337 §§5–6). An exit is the
sandbox's own power-down, not an instruction from outside, so it does not
cancel a wake. Confirming exit does not consume sleep requests. They live in
`/run`, and the stop clears them.

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

The image's `discobox` skill, which every harness reads, says that exiting
the primary terminal's shell stops the box within about half a minute,
whatever else is attached or held, and that quitting the harness alone does
not. It also says that the next attach starts the box again and resumes the
harness.

## Alternatives rejected

**The harness ending is an exit too.** Under this alternative, the shim would
learn the harness's process group (announced by `discobox-primary-run` before
it `exec`s the harness) and treat that group emptying as an exit, so quitting
Claude Code stops the box. Rejected by the owner (2026-11-16): quitting the
harness puts the person at the shell, which is where they expect to be. Only
leaving the shell, which also disconnects them, means they are done. This
alternative would also stop the box under a person who quit the harness to
use the shell. It needs a new announcement from the runner and a group record
in the shim, and it needs exceptions for suspends and for a failed
pre-harness hook.

**Type `<harness>; exit`, so the shell ends with the harness.** Rejected for
the same reason: it takes away the shell a person quits to. On bash it would
also exit when the harness is suspended, because bash runs the next command
in a list on Ctrl-Z.

**Report it as `idle`, with the cause in `lastActivity`.** No new reason
would be needed. It was rejected because idle's confirmation is refused while
any client is connected, including one attached to another terminal, which
should not hold up a box whose primary the person has left. Clients, the
runtime and the logs would also read one reason for two different claims.

**Report it as `sleep`.** On Docker that freezes the sandbox, keeping in
memory a box nobody is in. It also waits out sleep's grace.

**The agent powers itself off even under control-plane power.** That is
immediate, but on a boxd machine powering the guest off from inside does not
stop the machine (26-10-16-337, Context). It would also bypass the power lock
that serializes a stop with an attach's start.

## Consequences

- Exiting the primary terminal's shell stops its box about one status poll
  (15 s), a push and a reconcile later. Before this change the box waited out
  the idle timeout (30 minutes by default).
- Quitting the harness changes nothing. A box left at the primary's prompt
  idles out on the pool's timeout as before.
- Background work does not keep the box up once the primary's shell exits: a
  build in another terminal, a declared service, a held lease. To keep such
  work running, a person leaves the shell open, or detaches without exiting.
- The next attach starts the box and revives the primary through the
  harness's relaunch command (ADR 0038).
- The API contract gains `primaryExitedAt` on `SandboxAgentAutostopStatus`
  and `exit` in the confirm request's `reason` enum. The provider contract's
  `PowerDownReason` and the pool agent's gain `exit`. Neither gains a new
  method. No shim or runner change is needed: the exec's end is already
  recorded.

## Deferred

- **Waking the pool at once on exit**, as a port change does
  ([26-11-03-815](26-11-03-815-a-pool-waits-on-a-sandboxs-ports-and-reports-a-change-at-once.md),
  [26-11-09-062](26-11-09-062-a-harness-change-wakes-the-pool-like-a-port.md)),
  instead of at its next 15 s poll. Revisit if the poll's latency shows up as
  a cost or a complaint.
- **Terminals other than the primary.** Exiting a second terminal stops
  nothing. Revisit if a box is ever driven from a terminal that is not its
  primary.
