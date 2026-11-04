# 26-11-04-746 — A sleep request has a fifteen-second grace

- **Status**: Proposed (narrows [26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md) §§1, 4, 5, 8: sleep overrides activity only after its grace; sleep and idle reporting are gated during grace)
- **Date**: 2026-11-04
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

**An ordinary sleep request has a fixed fifteen-second grace. After that grace,
sleep continues to override activity. This provides time to finish a closing
response; it does not promise that the harness or background work has finished.**

### 1. The request files define the deadline

Each regular, non-dot file in `/run/discobox/sleep` becomes eligible fifteen
seconds after its mtime. The pending set's deadline is the latest of those
times: a new request or a refreshed file starts its own full grace, and an
older request cannot consume it early. Removing a file withdraws that request;
removing all of them cancels sleep.

`discobox-power sleep` continues to write a file with the current mtime, as does
`touch /run/discobox/sleep/agent`. No new command, flag, configuration setting,
API field, or database state is needed. This uses the existing file timestamps:
backdating a request can deliberately shorten its remaining grace, and a future
mtime delays it. It is a scheduling convention for a box's own power, not a
security boundary. Agent restart does not restart the grace; an already old
request is eligible when read again. A frozen box's files are consumed before
freeze as today, so thaw does not repeat the request.

### 2. The sandbox owns eligibility and rechecks it at confirmation

The policy reads the files on each evaluation. While requests exist but their
deadline has not arrived, it reports neither eligible sleep nor an idle
power-down: idle stop must not bypass the grace. Ordinary idle timeout behavior
resumes if all requests are removed.

Once eligible, sleep is reported through the existing status shape. The
server's existing power reconcile can act on it without a delayed job or a
second clock in the server. Under the pool's existing power lock,
`Confirm(ReasonSleep)` reads the pending set again. An absent set or a deadline
still ahead refuses confirmation and consumes nothing; a later poll can try
again. A successful confirmation consumes the pending requests and returns the
earliest wake as today. `Confirm(ReasonIdle)` must also refuse while a sleep
request is pending, so an earlier idle report cannot bypass the grace.

Before control-plane handover, the local policy uses the same eligibility
rule before consuming requests and invoking power-off. A pending grace must
also prevent its local idle fallback. Polls and reconciliation can make sleep
later than the deadline; fifteen seconds is a minimum for a normally dated
request, not a promise of exact shutdown time. Concurrent file changes retain
the existing filesystem snapshot semantics; no atomic transaction across
request writers and confirmation is promised.

### 3. Activity does not extend the deadline

The primary's `working`, output, an attached client, a keepalive lease, and
background commands do not extend a request's grace. A harness that reports no
status gets the same window. This preserves 26-10-16-337's reason for explicit
sleep: an attached console must not keep a finished worker up indefinitely.
A person issuing stop, restart, or suspend from outside does not wait for this
grace. Runtime choice remains 26-10-16-420's: Docker freezes a confirmed sleep
and falls back to stop if it cannot freeze.

## Alternatives rejected

- **Wait until primary idle for a quiet interval, with a ceiling.** Better for
  a long closing response when status is reliable, but the meaning and freshness
  of a terminal's status varies across harnesses, and background work may
  outlive an idle primary. A ceiling still cuts work off, while adding two
  timing policies and a status-dependent path. A fixed grace is the owner's
  suggested bounded first change; it makes no claim of detecting completion.
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

A usual sleep spends at least fifteen more seconds running. Work lasting
longer can still be frozen or terminated. Pending sleep temporarily suppresses
idle shutdown too; repeatedly refreshing requests can defer sleep, as a box
can already keep itself up with activity or leases.

Implementation must cover the eligibility boundary, multiple and refreshed
requests, cancellation, stale idle and sleep confirmations, local power-off,
and agent restart. Runtime evidence must record a busy primary, request time,
confirmation and actual power-down time, including output and a command still
in flight; the investigation's fixtures do not replace that evidence. Update
the in-box skill and command's timing text with implementation: replace
"whatever else is going on" with the grace and its bounded guarantee. Current
`DESIGN.md` files remain descriptions of the existing implementation until then.

Defer a completion handshake or status-dependent extension until observed
closing responses regularly exceed the grace or a user needs completion rather
than a bounded delay. Defer lossless harness-transition reporting until a
consumer needs every transition between status samples; a resumable stream of
sampled state cannot supply transitions the pool never observed.
