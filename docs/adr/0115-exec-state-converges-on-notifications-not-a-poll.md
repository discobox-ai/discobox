# 0115 — Exec state converges on notifications, not a poll

- **Status**: Accepted
- **Date**: 2026-09-12
- **Relates to**: [ADR 0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md), whose
  status poll is one of the callers that pays for this; [ADR 0028](0028-exec-log-transcripts-persist-as-compressed-sqlite-rows.md)
  and [ADR 0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md),
  which make an exec's recorded end outlive its run; [ADR 0046](0046-listening-ports-are-polled-and-probed-in-the-background.md),
  the standing poller this deliberately does *not* imitate;
  [ADR 0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md) §2, whose
  activity evaluation reads exec state on its own interval.

## Context

The sandbox-agent learns that an exec ended by asking, repeatedly, in a shell.

`execReconcileLoop` (`sandbox-agent/server/server.go:527`) ticks every two
seconds and calls `execs.Manager.Reconcile`, which walks every runtime JSON file
under `/run/discobox/agent-terminals` and calls `refreshExec`
(`execs/manager.go:1046`) on each. `refreshExec` forks `systemctl show` for ten
properties — `Id`, `LoadState`, `ActiveState`, `SubState`, `MainPID`,
`ExecMainPID`, `ExecMainStatus`, `Result`, `ActiveEnterTimestamp`,
`InactiveEnterTimestamp` (`execs/systemd.go:105`) — and, when the shim's socket
exists, dials it for the authoritative exit status and the terminal's title,
attacher count, and last access. It then writes the runtime file and calls
`store.ObserveExec`.

### What the poll is for

`ObserveExec` (`store/store.go:141`) upserts the exec's state row
unconditionally, but emits an event on only three transitions: `exec.observed`
(first sighting), `exec.status.changed`, and `exec.exited` (the first time
`ExitedAt` becomes non-nil). One further fact is computed only on this path —
the `lost` verdict at `manager.go:1063`, where a unit that is no longer *loaded*
(not merely inactive) means the shim cannot come back.

So the loop exists for one reason: to notice an exec's lifecycle transitions
when no client is asking. Without it, a declared service that died overnight
([ADR 0070](0070-services-are-declared-execs-the-sandbox-starts-for-you.md))
stays `running` in the store until the next read, and its `exec.exited` event
carries the wrong time or never lands at all.

That requirement is real. The polling is what is wrong with it.

### What the poll costs

The work is quadratic in the wrong places, because three call sites each refresh
what the layer beneath them already refreshed:

- `Manager.Reconcile` (`manager.go:509`) iterates `runtimeExecs`, which already
  calls `refreshExec` on every entry, and calls `refreshExec` on each again —
  **2N forks per tick**, half of them pure duplication.
- `SystemdRunner.List` (`systemd.go:133`) runs `systemctl list-units`, then forks
  `systemctl show` **per listed unit** (`systemd.go:147`) — `1+N`, not 1.
- `Manager.List` (`manager.go:469`) calls `runtimeExecs` (N), then `units.List`
  (1+N), then `refreshExec` per exec (N): **3N+1 forks per call**. Its callers
  are `autostop` every 30 seconds (ADR 0108 §2), `agentstatus` on every
  pool-agent status poll (ADR 0030), and every API list.

For a sandbox with N live execs the reconcile loop alone is **N short-lived
processes per second**, every one of them connecting to systemd, making one
round trip, and exiting. A host running a dozen sandboxes pays that N times
over; a snapshot of one such host showed 128 concurrent `systemctl` processes,
many of them blocked.

And in steady state every one of those forks computes the same answer: nothing
changed. A terminal sitting idle for six hours is polled ~10,800 times to learn
that it is still running.

### Both real transitions already push

Nothing about the requirement demands a poll, because the two events that can
actually change an exec's state are both announced:

- **The shim writes its own exit.** `wait()` records the final status and writes
  the runtime file (`execs/shim.go:461`) as soon as the command exits. The
  normal path is already a push; the agent just isn't listening.
- **systemd knows when a unit dies**, including every case the shim cannot
  report — killed shim, OOM, a unit stopped from outside, a unit that did not
  survive a reboot. It will say so over D-Bus.

`fsnotify` and `github.com/coreos/go-systemd/v22` are both already direct
dependencies of the `sandbox-agent` module.

## Decision

### 1. The shim's runtime write is the exit notification

A watcher on the runtime directory (`fsnotify`, as `sourcesready` and
`secretswatch` already use it) reacts to a write of `<exec-id>.json` by reading
that one file and observing it. `/run` is tmpfs; inotify works there.

This covers the ordinary end of every exec — a command that finishes, a service
that exits, a terminal whose shell is closed — with no interval and no
subprocess, and it observes the exit at the moment it happened rather than up to
two seconds later.

`refreshExec` also writes the runtime file (`manager.go:1094`), so the watcher
must not treat the agent's own writes as news. The refresh path writes only when
the file would actually differ — and "differ" is decided on the exec's *durable*
state, because the fields the shim reports live cannot be part of that test:
`LastAccessedAt` is `time.Now()` for as long as any client is attached, and a
harness animating its title moves `TitleChangedAt`. Comparing the whole record
would therefore find a difference on every refresh of an attached terminal, and
each write would wake the watcher into another refresh — a self-sustaining loop
doing far more work than the poll it replaced, for exactly as long as somebody
has a terminal open. Those fields are live facts rather than durable ones, so
the runtime file simply does not carry them.

### 2. systemd's D-Bus signals are the notification for everything else

`SystemdRunner` holds one `go-systemd/dbus` connection for the life of the
agent, calls `Subscribe()`, and consumes `PropertiesChanged` for
`discobox-exec-*` units. `JobRemoved` is *not* consumed: go-systemd handles it
inside its own dispatch loop to complete the job channels `StartTransientUnit`
and `StopUnit` wait on, and it never reaches the watcher. That is not free —
once any subscriber is installed, go-systemd's dispatcher makes a synchronous
`GetUnit` call for every `JobRemoved` in the sandbox, including jobs that have
nothing to do with execs. It is a round trip on an open socket rather than a
process, and avoiding it would mean consuming raw godbus signals instead of the
library's subscriber API. A signal for a unit resolves to its exec and
observes it, fetching the remaining properties — `ExecMainStatus`, `Result`,
`MainPID`, the timestamps — with `GetAllProperties` on that one unit. That is a
socket round trip on an open connection, not a process.

This is what catches the cases §1 cannot: the shim killed before it could write,
the unit gone entirely, the `lost` verdict at `manager.go:1063`.

What it is *not* is a source of exit status. A transient unit is collected as it
dies, so by the time its final signal can be acted on, `LoadState` already reads
`not-found` and `ExecMainStatus`/`Result` read `0`/`success` whatever the
command did — measured, not assumed: a probe unit whose script exited 7 reports
exactly that. The unit answers one question, "is it still there", and §1
answers the other. Neither mechanism is redundant with the other.

It must be the **system bus** (`NewSystemConnectionContext`). The private socket
at `/run/systemd/private` needs no dbus-daemon and is the better choice for
method calls, but `Subscribe()` registers through `AddMatch` on the bus object,
which only a dbus-daemon serves. `dbus` is installed by `base-image/Dockerfile`
and is not among the units the image masks, and `dbus.service` is active in a
running sandbox.

### 3. A sweep runs at boot, and otherwise only as a declared fallback

One `ListUnitsByPatterns("discobox-exec-*")` at startup reconciles everything
that happened while the agent was down — the post-reboot case that produces most
`lost` verdicts — and there is no periodic sweep after it.

If `Subscribe()` fails, the agent logs that it is degraded and falls back to a
single batched sweep on a 60-second interval: one `ListUnitsByPatterns` plus a
property fetch per unit, over the same connection, still with no forks. This is
a genuine runtime capability that may be absent, which is the one case the
repository's rules allow optionality for — not a seam to avoid updating callers.

### 4. `SystemdRunner` stops shelling out

`Start`, `Stop`, `Status`, and `List` move onto the D-Bus connection:
`StartTransientUnit` for `Start` (which is what `systemd-run` does), `StopUnit`,
`GetAllProperties`, `ListUnitsByPatterns`.

`UnitManager` (`manager.go:129`) gains `Watch`, and every implementation
including the test fakes implements it. It is a required method rather than an
optional interface a runner may or may not satisfy: exec state now *converges*
on these notifications, so a unit manager that cannot report a change is not a
lesser unit manager but a broken one — every exec that ended would stay pinned
to its last observed status. An implementation with nothing to report returns a
channel it never sends on.

Unit names are stored bare and given their `.service` suffix only at the D-Bus
boundary. `systemd-run` appended it implicitly and the suffix then came back on
`Id` and was stored, which `nextUnitGeneration` — it parses `<base>-g<N>` — could
not read: a second relaunch would have reused generation 2's unit name. Naming
the suffix explicitly is what a D-Bus call needs anyway, and normalizing on the
way in settles that latent collision.

`SystemdRunner.List` stops forking `systemctl show` per unit. `Manager.List`
stops refreshing what `runtimeExecs` refreshed — the duplicated `refreshExec`
calls at `manager.go:483`, `:487`, and `:511` go away, and `Manager.Reconcile`
is deleted along with the loop that drove it.

### 5. Nothing above `execs` changes

`ObserveExec` and its three events are unchanged; only their trigger moves.
`autostop`, `agentstatus`, and the API keep calling `List` and `Get` and keep
getting a current answer — now assembled from an in-process view that
notifications keep fresh, rather than from forks charged to the caller. No API
contract changes, no stored state changes, no pool-agent or server change.

## Alternatives rejected

**Keep the poll, batch it into one call.** The obvious fix: one
`systemctl show` for every unit per tick, N forks becoming 1. Rejected because
it optimizes the wrong thing — it still asks a question every two seconds whose
answer is almost always "nothing," and it leaves the answer up to two seconds
stale. Subscribing costs about the same code and removes the interval entirely.

**fsnotify alone, with no D-Bus.** Tempting, because it covers the common path
with one small watcher and no new connection. Rejected because it cannot see a
shim that died without writing, which is exactly when an exec's state is most
wrong, and it has no answer at all for `lost`: a unit that vanished across a
reboot leaves a runtime file that never changes again.

**D-Bus alone, with no fsnotify.** Rejected because the shim is the authority on
exit status, not the unit (`manager.go:1077`): the shim lingers after its command
so a late attacher can read the exit code, so the unit is still active when the
command has already finished. A unit signal would make the agent go ask the shim;
the shim's own write is the earlier and more accurate signal, and it is free.

**`go-systemd`'s `SubscribeUnits`.** The library's own convenience API. Rejected
because it polls `ListUnits` on an interval internally and diffs the result —
the design being replaced, one layer down, with less control over which
properties are fetched.

**The private socket for signals.** It needs no dbus-daemon, which would be one
fewer thing to depend on inside the sandbox. Rejected as unavailable: signal
registration goes through the bus daemon's `AddMatch` (§2). Method calls could
use the private socket and signals the system bus, but two connections to serve
one component is not worth avoiding a dependency the image already ships.

Re-examined once the consequence above was understood — `systemctl` reached
systemd without a dbus-daemon, so this moves *queries*, not just notifications,
onto the bus — and kept. What made it a correctness question was exec state
being demoted to `lost` on a failed query; with that fixed, an outage is stale
state that recovers on its own, and the remaining difference is availability. One
connection with one lifetime and one failure mode is worth more than the window
it would close.

**The shim pushing to the agent over HTTP instead of writing a file.** A direct
notification with no watcher. Rejected because the shim already writes the file
for its own reasons — it is the exec's durable runtime state — so a push would be
a second mechanism carrying the same fact, and one that is lost if the agent is
restarting when the shim exits. The file is still there when the watcher comes
back.

**Just lengthen the interval.** Ten seconds instead of two would cut the forks
fivefold for one line of diff. Rejected because it trades the accuracy of
`exec.exited` timestamps for the saving, and leaves a design that gets worse
with every exec a sandbox runs.

## Consequences

- The steady-state cost of a live exec drops to zero processes and zero timers.
  The reconcile loop's `N` forks per second per sandbox disappear; so do the
  `3N+1` forks that `Manager.List` charged to `autostop`, `agentstatus`, and
  every API listing.
- `exec.exited` is recorded when the exec exits rather than up to two seconds
  later, and `exec.status.changed` follows the unit's actual transitions instead
  of a sampling of them.
- The sandbox-agent now depends on `dbus.service` for more than notification.
  `systemctl` reached systemd through `/run/systemd/private`, which needs no
  dbus-daemon; every method call now goes over the system bus, so a bus outage
  fails `Status` and `List` as well as stopping signals. What bounds the damage
  is that a status *error* leaves an exec's status alone: only `Loaded` demotes
  an exec to `lost`, which is the rule `DESIGN.md` already stated and which the
  polling code did not honour — it treated "could not ask" as "unit is gone",
  and the terminal layer revives a lost exec by stopping its unit first. Without
  that fix a bus hiccup would have killed and relaunched every live terminal in
  the sandbox. With it, an outage leaves exec state stale and recoverable, which
  is what §3's fallback is for. This is still a new hard-to-see failure mode,
  and the reason the fallback is declared rather than silent.
- One long-lived D-Bus connection must survive a systemd reload and reconnect if
  it drops; a dropped connection that is not noticed is a silent stop to all
  convergence. The subscription therefore checks that its connection is still up
  on a 30-second timer — a liveness check on one socket, not a query about any
  exec — and ends itself when it is not, which is what makes §3's fallback
  reachable rather than theoretical. The watcher then tries to re-subscribe on
  each fallback tick, so degraded is a state to leave rather than settle into.
- `systemd-run` is no longer invoked as a binary, so a unit's start options move
  from command-line flags into `StartTransientUnit` properties. This is the
  largest single piece of the change and the one most likely to need care:
  `KillMode=control-group`, `WorkingDirectory`, and every `--setenv` become
  typed properties.
- `execs` tests that fake `UnitManager` are unaffected (§4). Tests that assert on
  the reconcile loop's timing lose their subject and are replaced by ones that
  deliver a notification.
- `sandbox-agent/DESIGN.md`'s "Treat systemd as the source of truth for terminal
  unit liveness" rule stays true and gains a sentence about how liveness now
  arrives. That edit rides the implementation, not this ADR.

## Deferred

- **The pool-agent's own status poll** (ADR 0030). It polls every sandbox on an
  interval and will keep doing so; this decision only makes each poll cheap to
  answer. Revisit if the polling itself, rather than what it costs the
  sandbox-agent, shows up as load on a busy pool host.
- **`ports`' standing poller** (ADR 0046). It samples `/proc/net/tcp{,6}`, which
  has no notification to subscribe to, so it stays a poll. Unrelated to this
  decision beyond the resemblance.
- **`autostop`'s 30-second evaluation** (ADR 0108 §2). Its inputs — title
  changes, attacher counts, lease mtimes — are not exec lifecycle transitions,
  and the interval is the policy rather than a sampling strategy. It gets a
  cheaper `List` and nothing else.
- **Whether the shim should stop lingering.** The linger is why the unit outlives
  the command and why §1 and §2 are both needed. Revisit only if
  `execstream`'s exit retention moves somewhere else.
