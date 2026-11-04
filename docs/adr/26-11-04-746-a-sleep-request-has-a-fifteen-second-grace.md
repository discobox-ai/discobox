# 26-11-04-746 — A sleep request has a fifteen-second grace

- **Status**: Accepted (narrows [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md) §§1, 4, 5, 8: sleep waits for a minimum grace and bounded OSC 7501 working deferral; sleep and idle reporting are gated while waiting)
- **Date**: 2026-11-04
- Accepted by the owner by merging #565, 2026-11-04
- **Relates to**: [#564](https://github.com/discobox-ai/discobox/issues/564);
  [26-10-16-420](26-10-16-420-a-docker-pool-freezes-a-sleeping-sandbox-and-a-person-can-suspend-one.md)
  §2, Docker's freeze with stop fallback;
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md)
  §4, reported work as activity;
  [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md), sampled status;
  [26-10-29-316](26-10-29-316-sandbox-state-changes-are-a-resumable-event-stream.md)
  §1, program state only while running;
  [26-11-01-560](26-11-01-560-discobox-box-watches-other-boxes-through-the-sandbox-event-stream.md), the box monitor.

## Context

An agent commonly asks to sleep in its last tool call and still has a closing
response to write. Today a sleep file overrides every activity signal at once,
including a primary terminal reporting `working`. A freeze can preserve a turn
half written; the shutdown fallback can terminate it.

The [investigation](https://github.com/discobox-ai/discobox/issues/564#issuecomment-6622449852)
exercised `sandbox-agent/autostop` with a supplied primary exec reporting
`working`: `Evaluate` returned `idle=false`, yet `Confirm(ReasonSleep)` succeeded
in 80 microseconds. Its local `Run` path called the injected power-off callback
about 1.5 ms after the request with the test's 1 ms interval. These are policy
probes, not measurements of an actual harness or runtime freeze. A full dev-box
reproduction was unavailable because the dev server could not start without
its local platform/harness bundles. The production local interval is 30 s;
the pool samples status every 15 s, with no minimum wait from a sleep request.

The monitor's missing idle events cannot resolve whether a historical freeze
interrupted a turn. Status is sampled: a turn may end between the report that
asked for sleep and power-down, without an idle sample reaching the server.
The store omits program state when the box is suspended, and the monitor does
not invent idle from that absence. A controlled monitor stream containing idle
and suspended in the same run emitted both events; without an idle event it
emitted only the power change. The evidence establishes both possible paths,
not which path each of the historical 87 transitions took.

## Decision

**An ordinary sleep request waits at least fifteen seconds, then continues to
wait while the primary terminal reports OSC 7501 `working`, up to two minutes
from the latest request. It may sleep sooner after the minimum once `working`
is no longer reported. This protects a closing response with a bounded wait;
it does not promise that the harness or background work has finished.**

### 1. The request files define the minimum and ceiling

Each regular, non-dot file in `/run/discobox/sleep` asks to sleep. From the
latest mtime in the pending set, the minimum is fifteen seconds and the ceiling
is two minutes. A new request or refreshed file restarts both windows; an older
request cannot consume it early. Removing a file withdraws that request;
removing all of them cancels sleep.

Before the minimum, sleep is never eligible. Between the minimum and ceiling,
it is eligible only if the primary terminal's effective OSC 7501 status is not
`working`. At or after the ceiling it is eligible regardless of that status.
The ceiling bounds eligibility, not actual power-down latency: the next poll,
reconcile and confirmation still have to run.

`discobox-power sleep` continues to write a file with the current mtime, as does
`touch /run/discobox/sleep/agent`. No new command, flag, configuration setting,
API field, or database state is needed. This uses the existing file timestamps:
backdating a request can deliberately shorten its remaining grace, and a future
mtime delays it. It is a scheduling convention for a box's own power, not a
security boundary. Agent restart preserves the request timestamps and both
windows; eligibility is re-evaluated with fresh primary status. A request past
the minimum but before the ceiling still defers for `working` or a status read
failure. Only an elapsed ceiling permits eligibility regardless of status.
A frozen box's files are consumed before freeze as today, so thaw does not
repeat the request.

### 2. The sandbox owns eligibility and rechecks it at confirmation

The policy reads the files on each evaluation. While requests exist but sleep is
not yet eligible under both timing and status rules, it reports neither
eligible sleep nor an idle power-down: idle stop must not bypass the wait.
Ordinary idle timeout behavior resumes if all requests are removed.

Once eligible, sleep is reported through the existing status shape. The
server's existing power reconcile can act on it without a delayed job or a
second clock in the server. Under the pool's existing power lock,
`Confirm(ReasonSleep)` reads the pending set and the primary
terminal's live effective status again. An absent set, an unelapsed minimum,
or `working` before the ceiling refuses confirmation and consumes nothing;
a later poll can try again. A primary that resumed working after the report
is therefore protected until the ceiling, without relying on server-cached
status. A successful confirmation consumes the pending requests and returns the
earliest wake as today. `Confirm(ReasonIdle)` must also refuse while a sleep
request is pending, so an earlier idle report cannot bypass the wait.

Before control-plane handover, the local policy uses the same eligibility
rule, including a fresh primary status check, before consuming requests and
invoking power-off. A pending wait must also prevent its local idle fallback.
Fifteen seconds is a minimum for a normally dated request, not a promise of
exact shutdown time; a continuously `working` primary permits sleep at two
minutes even if the idle timeout is much longer. Concurrent file changes retain
the existing filesystem snapshot semantics; no atomic transaction across
request writers and confirmation is promised.

### 3. Only effective OSC 7501 `working` extends the minimum

Read the primary terminal's effective status from the sandbox's existing shim
status, using its primary-terminal identity and OSC 7501 record aggregation.
Do not parse terminal text or add a second OSC parser. The shim already handles
record lifetime and effective status under 26-10-16-424 §§2–3. This is the
state `working`, regardless of whether a numeric progress value is present;
a progress field alone, or another OSC 75xx message, is not a busy signal.

An effective `idle`, `blocked`, `done`, or `error` does not defer sleep beyond
the minimum. Neither does an absent report or a missing primary: absence is
not proof of completion, but a harness without status must still be able to
sleep. Requiring no OSC status at all would keep a healthy idle harness up
because its record can remain present. The primary need not report idle for
another quiet interval; the fifteen-second minimum is the closing window.

Screen output, attached clients, keepalive leases, secondary terminals, and
background commands do not extend the minimum. Only effective primary
`working` does, and only until the ceiling. A status read failure must not be
treated as an observed absence: before the ceiling defer eligibility and
refuse confirmation; at the ceiling sleep may proceed. The request remains
pending for a later retry. No status-report age heuristic expires `working`
early; the ceiling bounds a stuck report. A final status check is not a lock
on harness execution, so work that starts after confirmation can still be
interrupted.

A person issuing stop, restart, or suspend from outside does not wait for this
policy. Runtime choice remains 26-10-16-420's: Docker freezes a confirmed sleep
and falls back to stop if it cannot freeze.

## Alternatives rejected

- **Fixed fifteen-second grace regardless of OSC 7501 state.** Simple, but
  ignores a direct report that the closing turn is still working. Use that
  first-hand signal to defer further, with a ceiling for stuck reports.
- **Require a reported idle state or no OSC status at all.** A healthy idle
  record can persist; an uninstrumented harness may never report idle. Neither
  should prevent sleep. Test specifically for effective `working` instead.
- **Require a quiet interval after `working` ends.** Adds another timer without
  proving background work is finished. The minimum grace and fresh confirmation
  provide the chosen protection; no second quiet timer is introduced.
- **Wait for a harness turn-exit handshake.** This could make completion the
  gate, but requires a new cross-harness lifecycle contract, including missing
  or failed hooks and commands that continue after a turn. Sleep is currently
  available to every process, including a plain shell. That larger contract
  is not justified by this investigation.
- **Delay only in `discobox-power` or only in the server.** A helper delay misses
  direct file writers; a server delay misses local power-off and cannot replace
  the fresh confirmation when a request changes.
- **Keep immediate sleep and change the monitor to synthesize idle.** The policy
  probe demonstrates a real interruption path. Inventing idle would hide it
  and contradict the program-state contract.

## Consequences and deferred work

A usual sleep spends at least fifteen more seconds running. A primary reporting
`working` can keep it up until two minutes after the latest request. Work
lasting beyond the ceiling, or not reflected in primary status, can still be
frozen or terminated. Pending sleep temporarily suppresses idle shutdown too; repeatedly refreshing requests can defer sleep, as a box
can already keep itself up with activity or leases.

Implementation must cover the eligibility boundary, multiple and refreshed
requests, cancellation, stale idle and sleep confirmations, local power-off,
and agent restart. Cover `working` across the minimum and ceiling, transition
out of and back into `working`, all non-working and absent states, numeric
progress without effective `working`, secondary-terminal activity, and status
read failures before and at the ceiling. Runtime evidence must record a busy
primary, request time,
confirmation and actual power-down time, including output and a command still
in flight; the investigation's fixtures do not replace that evidence. Update
the in-box skill and command's timing text with implementation: replace
"whatever else is going on" with the grace and its bounded guarantee. Current
`DESIGN.md` files remain descriptions of the existing implementation until then.

Defer a completion handshake until a user needs completion rather than this
bounded deferral, or observed closing responses regularly exceed the ceiling.
Defer lossless harness-transition reporting until a consumer needs every transition between status samples; a resumable stream of
sampled state cannot supply transitions the pool never observed.
