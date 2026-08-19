# 0060 — Provisioning progress is a recorded phase, polled by the waiting client

- **Status**: Accepted
- **Date**: 2026-08-19
- **Relates to**: [ADR 0039](0039-attach-waits-for-readiness-at-every-tier.md),
  whose tiered waits and reporting channel this keeps, and whose progress
  *transport to the client* it replaces.
  [ADR 0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md) owns the
  other observation channel out of a sandbox, and is why tier 3 stays
  unreported below. [ADR 0034](0034-sandbox-state-and-runtime-state-are-separate-fields.md)
  §2 keeps progress off both state axes.

## Context

ADR 0039 decided that a client creating a sandbox and attaching to it issues one
POST and one websocket, with each tier waiting for the readiness only it can
observe. It also decided how that wait is made legible: progress is an opt-in
frame on the attach connection, emitted by tier 1, which stops being a reverse
proxy and becomes a relay.

**Everything below the client landed.** The pool agent aggregates the daemon's
pull stream for a status line (`pool-agent/sandboxruntime/pullprogress.go`),
reports it on the sandbox-states channel in its own `progress` array, and the
control plane records it as `runtime.provisionProgress` and publishes a project
event (`Store.ApplySandboxProgressReports`).

**The client-facing half did not.** `execstream/frame` has no progress type, and
the attach route still waits in `AwaitSandboxHTTPClient` and then hands the
request to `httputil.ReverseProxy`. The result is precisely the silent socket
ADR 0039 set out to prevent: the client blocks inside `websocket.Dial` because
no 101 has been sent, `disco run` prints one line and waits, and the launcher
sits on `attach…` with no way to tell a slow image pull from a hang.

Two things have to be decided to close that gap: what the system *records* about
a provisioning sandbox, and how a waiting client *reads* it.

### The relay is more than the status line is worth

Building the frame means the control plane terminates the attach websocket:
accept the upgrade before the wait, read and buffer client frames throughout it,
dial the sandbox agent itself, then copy both directions. Four consequences
follow that ADR 0039 did not weigh.

- **The resume handshake is not progress-tolerant.** `resume.Conn.establishLocked`
  reads exactly one frame and requires `SessionOK`; a progress frame arriving
  there fails the attach as `ErrProtocol`. The client writes its `Session` frame
  the instant the dial returns, so this is the ordinary case, not an edge.
- **The relay must read while it waits.** coder/websocket answers pings only
  from a read path. A relay that upgrades and then sits in a provisioning-length
  wait without reading stops answering the CLI's keepalive, and the client tears
  down the attach it is waiting on.
- **Failures lose their status code.** Today a refused attach is an HTTP status
  and a JSON body the CLI renders as a real reason. After the upgrade there is
  no status to send.
- **Opt-in does not contain the blast radius.** The CLI is the client that would
  opt in, so the relay lands on the path of every interactive attach and every
  `resume` reconnect. It also shortens the transport heartbeat: `execstream/resume/DESIGN.md`
  states that probe covers the route from the CLI through the control plane and
  pool transport to the sandbox agent, and terminating at tier 1 quietly reduces
  it to the first hop while the telemetry keeps reporting.

### The project event stream is not a foundation to build on

The obvious alternative — have the client watch the sandbox on the project event
stream — reads well and does not survive contact with the implementation. The
in-process broker publishes with a non-blocking send and a bare `default:`
(`server/internal/events/broker.go`), so a subscriber whose buffer fills loses
events silently. The client-facing stream stamps every message with `seq` but
offers no resume-from-seq: a client can detect a gap and cannot close it. It was
built as a Kubernetes-style list-then-watch, the listwatch half never
materialized, and its only consumer in the repository is the `disco events`
debug command.

ADR 0039's tier-1 wait uses that broker correctly and is unaffected: it treats
an event purely as a wake-up, re-reads authoritative state on every wake, and
keeps a recheck timer precisely because it knows events are dropped. That is the
discipline a lossy hint channel demands, and it is a poor fit for a status line
that must not stick on a stale phase when the last event is the one dropped.

## Decision

**What the system records about a provisioning sandbox is the contract. A client
that wants to narrate the wait polls the sandbox for it while its attach
blocks.** Nothing is added to the attach protocol; the control plane's attach
route stays a reverse proxy.

### The record carries a phase, not just a pull

`provisionProgress` stops being pull-shaped. It carries a **phase** — what is
being done — with pull byte counts as one optional refinement, so a client
renders a useful line at every stage rather than only at the one stage that
happens to have a denominator.

The pool agent reports a phase at each boundary `CreateSandbox` already has:
resolving and pulling the image, preparing the sandbox's volumes, materializing
a pushed source, creating the container, starting it, and waiting for the sandbox
agent to answer. These stay observations in exactly the sense ADR 0039
established — carried in the `progress` array of the sandbox-states report,
applied separately from state, marking nothing dirty, taking no part in the
complete-sync rule.

This is the durable half of the decision. The transport above it may change
again; what a client can *know* about a provisioning sandbox is recorded on the
resource either way.

### The client polls, and only while it is waiting

A frontend narrating a wait reads the sandbox on a fixed short interval,
renders the phase, and stops the moment the attach returns. It is display only:
it gates nothing, and the attach is not waiting on it.

This is a poll, and ADR 0039 removed one. The distinction is what the poll is
for. ADR 0039 removed *readiness gating* — a loop that had to complete before
the client was allowed to attach, whose cost was a wasted round trip per second
for information the server would have volunteered. This loop runs beside an
attach that is already blocked and already correct, ends when the attach
returns, and its worst failure is a status line that updates late.

Polling is also the property that makes it robust: each read is the current
truth, so a missed update is not a lost one, and there is no gap to recover
from. That is the guarantee the event stream cannot give today.

### Phases the client owns, the client reports

Source delivery stays exempt from every server-side wait (ADR 0039), because a
push-delivered sandbox cannot start until this client pushes. The steps before
and during that push — resolving the source, snapshotting uncommitted work,
creating the sandbox, waiting for it to park on `awaiting_source`, pushing — are
the client's own and are narrated by the frontend directly. No round trip tells
a process what it is currently doing.

### Tier 3 stays unreported, for now

The harness install and primary-terminal launch inside the sandbox agent have no
channel to the control plane fast enough to narrate them: ADR 0030's status
poller runs on a 15-second interval and covers running sandboxes only. Rather
than build a third channel for the shortest stage, a client names it by
inference — the sandbox is ready and the attach has not yet delivered, so what
remains is the terminal.

Revisit when the sandbox agent gains a push channel to the pool agent for any
other reason, or when install time is measured to be a material share of the
wait rather than assumed not to be.

## Alternatives rejected

**Build the relay as ADR 0039 specified.** The user-visible result is the same
status line; the cost is not. It puts a new websocket termination and a
bidirectional copy on the path every keystroke of every attach travels, requires
the resume handshake and the CLI's attach error path to change, and shortens a
documented end-to-end latency probe to one hop. The frame's real advantages —
one connection, and progress bound to the attach's own lifetime — are worth
something, but not that.

**Watch the sandbox on the project event stream.** One connection, woken by the
write rather than chasing it, and the event payload is already the whole sandbox
row. Rejected because the stream cannot currently deliver what a status line
needs: a lossy fanout with sequence numbers no client can resume from means the
one dropped event that matters is the last one, which leaves the line stuck on a
stale phase with nothing to correct it. Revisit if that stream gains reliable
delivery or seq-resume; the recorded phase is the same either way, which is what
makes the transport replaceable.

**Record progress and let the launcher's list show it, with no poll.** The list
already refreshes on a tick, so it would narrate the wait for free. Rejected as
the whole answer because it leaves `disco run` exactly as silent as it is today,
and a foreground command blocking on a provisioning sandbox is the case that
hurts most.

**Put the phase on `Sandbox.State`.** It would ride the existing state channel
and need no new field. Rejected for the reason ADR 0034 separated the axes at
all: `state` is the reconciler's, is durable, and drives decisions. A pull in
flight decides nothing and is history the moment it ends.

## Consequences

- ADR 0039's "Progress is a frame, and only tier 1 sends it" is superseded, as
  is the closing sentence of its "Image pull progress" section. Every other part
  of ADR 0039 stands: the tiered waits, the nested stall budgets, progress
  refreshing them, and the reporting channel from the pool agent upward.
- `execstream` is untouched. The attach protocol, the relay that would have
  terminated it, and the transport heartbeat's end-to-end reach all stay as they
  are.
- A client narrating a wait issues a bounded series of single-row reads during
  provisioning and none afterward. The interval is a named constant, tunable
  without a protocol change — which is the point of keeping the contract on the
  record rather than on the wire.
- Progress is not bound to an attach. Two clients waiting on one sandbox both
  see it, a client that attaches without polling sees none, and the poll can
  outlive or predate the attach it accompanies. This is a real loss against the
  frame, and is accepted: the record describes the sandbox, not who is waiting.
- `provisionProgress` is phase-shaped rather than pull-shaped, so its consumers
  read a phase first and the pull detail only when the phase says there is one.
