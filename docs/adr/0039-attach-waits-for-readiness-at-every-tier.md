# 0039 — Attach waits for readiness at every tier

- **Status**: Accepted (the progress-frame transport of "Progress is a frame, and only tier 1 sends it" superseded by [0060](0060-provisioning-progress-is-a-recorded-phase-the-client-polls.md))
- **Date**: 2026-08-12
- **Relates to**: [ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §12, which put the on-demand start latch in the pool agent because it is the
  only process that knows the container's true state. This ADR extends that
  reasoning up and down the stack: each tier waits for the readiness only it
  can observe. [ADR 0022](0022-sandbox-deletion-is-archive-then-confirmed-purge.md)
  §5 keeps archived sandboxes exempt from every wait here, for the same reason
  it exempts them from auto-start.

## Context

Creating a sandbox and getting a terminal on it is one intention, but the
client executes it as a poll loop. `disco run` today:

1. `POST /projects/{p}/sandboxes` — create
2. `GET  /sandboxes/{id}` every second until `awaiting_source` (only when the
   source must be pushed), push, `POST .../complete-source-push`
3. `GET  /sandboxes/{id}` every second until `display_state=running`
   (`cli/internal/cli/sandbox.go:624`)
4. `GET  /sandboxes/{id}/execs` every second until a primary terminal exists
   and is past `installing` (`cli/internal/cli/run.go:176`)
5. `GET .../execs/primary/attach` — the websocket

Steps 3 and 4 exist only because the attach in step 5 fails on a sandbox that
is not ready. They are pure client-side compensation for a server that answers
"not yet" instead of waiting, and the cost is proportional to provisioning
time: a sandbox that takes 40 seconds to come up costs roughly 40 API calls
before the one call that matters. Every client must reimplement the loop, and
the launcher does not — it attaches immediately on create
(`cli/internal/cli/tui.go:331`, `cli/internal/tui/model.go:1173`), which is why
its attach lands on a sandbox that is still provisioning and reports a dead-end
error with nothing to retry.

Three separate gates make the attach fail early, each observable by exactly one
tier and by no other:

- **Control plane.** `AcquireSandboxHTTPClient`
  (`server/internal/resources/sandboxes/service.go:328`) needs the pool active
  and ready, and needs `Sandbox.RuntimeState` to name a pool. That blob is
  empty until the sandbox reconciler has created the runtime and persisted it
  (`server/internal/resources/sandboxes/reconciler.go:498`), so an attach
  issued the instant create returns fails in `poolIDFromRuntimeState` with
  `ErrNotFound` (`server/providers/poolruntime/provider.go:419`).
- **Pool agent.** `autoStart` → `EnsureSandboxRunning`
  (`pool-agent/server/autostart.go:32`, `pool-agent/sandboxruntime/power.go:61`)
  already starts a stopped container and blocks until the sandbox-agent answers
  `/healthz`. It does not wait for the container to *exist*: mid-create,
  `GetSandbox` returns `ErrNotFound`, autostart logs and falls through, and the
  proxy fails with "no inspectable IP address" about a fact the caller cannot
  act on.
- **Sandbox agent.** Attaching the virtual `primary` id already launches the
  primary terminal when it is absent, blocks through hook and file install, and
  starts it (`sandbox-agent/terminal/service.go:298,348`); `checkAttachable`
  deliberately admits a created-but-unstarted exec because the shim dial waits
  for the socket (`sandbox-agent/execs/manager.go:558`). But that wait is a
  5-second dial timeout (`sandbox-agent/shimproxy/shimproxy.go:22`), not a
  readiness budget, so an attach arriving while boot's `EnsurePrimary` is still
  installing a harness resolves onto that exec and then times out under it.

Nobody can see all three. The control plane cannot see the container; the pool
agent cannot see the primary terminal; the sandbox agent cannot see whether the
sandbox has been dispatched to a pool at all. A single wait, wherever it were
placed, would have to poll the tier below it.

## Decision

**Attach blocks for readiness at every tier, and each tier waits only for what
it alone can observe, under its own timeout.**

A client creates a sandbox and attaches. One POST, one websocket. No polling,
with the single exception below.

### Tier 1 — control plane

The exec-attach route waits until `AcquireSandboxHTTPClient` can succeed:
the sandbox has a runtime state naming a pool, and that pool is active and
ready. The wait is **event-driven, never a poll**: writes publish a project
event after commit (`server/internal/store/resource_events.go:37`), and the
in-process broker (`server/internal/events/broker.go`) already fans those out —
it is the same backbone `projectstream` serves the client's event stream from.
The handler subscribes, filters to this sandbox and its pool, and re-attempts
the acquire on each relevant event.

**The state-report path has to publish, and did not.** Pool-agent state reports
were written with a raw `tx.Model().Updates()` that bypasses
`withResourceEvent`, and `observationNeedsReconcile` deliberately ignores a
sandbox that came *up* ("nothing about the control plane's view of it changed
by its starting"). So the one transition this wait exists to observe — a
sandbox becoming usable — reached the database and nothing else: no event, no
reconcile, nothing for a waiting attach to wake on. `ApplySandboxStateReports`
now creates and publishes a project event when a report actually changes the
recorded state or error, which also fixes clients that could not see a sandbox
start on the event stream at all. Only real changes publish: the complete sync
re-reports every sandbox on its interval, and an event per sandbox per sync
would be a heartbeat wearing a resource event's clothes.

Terminal conditions end the wait immediately rather than burning the timeout:
desired state `deleted` and observed state `failed` are answers, and an
archived sandbox is answered as it is today (409, per ADR 0022 §5) rather than
waited on.

### Tier 2 — pool agent

`autoStart` waits for the container to exist before `EnsureSandboxRunning`,
then reuses the start-and-wait path already there. The existing
`waitForSandboxAgent` health probe (`pool-agent/sandboxruntime/runtime.go:1366`)
stays as-is: it is a loopback readiness probe on the pool's own network during
a start this tier initiated, not a client polling an API, and replacing it with
an event would mean inventing a readiness signal the sandbox-agent does not
emit today.

### Tier 3 — sandbox agent

The attach waits for the resolved primary terminal to be genuinely attachable —
launched, install finished, shim listening — under a timeout that covers
install rather than only the socket dial. This replaces the accidental
5-second budget with a stated one.

**The launch is single-flighted, and waiting is joining it.** `EnsurePrimary`
is a check-then-act with no lock: it scans `List()` for a primary in
`starting`/`running` and, seeing none, resolves the harness, reads
`PrimaryTerminalLaunched`, creates, installs, and starts
(`sandbox-agent/terminal/service.go:353-388`). The record only becomes visible
to a concurrent scan after `execs.Create` has resolved the user and command and
written the runtime file, and `execs.Manager` holds no in-process lock —
`List()` re-reads runtime files, durable records and unit status from disk on
every call. Boot runs this in a goroutine started immediately before the HTTP
server begins serving (`sandbox-agent/server/server.go:249`), so boot and a
first attach are concurrent by construction. Today the client's exec-list poll
hides it by never attaching until a primary already exists; removing that poll
is the whole point of this ADR, so the attach starts arriving squarely inside
the window. Two things then go wrong:

- **Duplicate primaries.** The attach's scan runs before boot's record is
  visible, so it launches a second primary terminal. Two processes are tagged
  primary and `selectLivePrimary` returns whichever it happens to find first.
- **The prompt runs twice.** Both callers read `PrimaryTerminalLaunched` as
  false before either calls `MarkPrimaryTerminalLaunched`, so both take
  `primaryCreateRequest`'s first-launch arm and pass the user's prompt as argv
  instead of one launching and the other resuming.

So the primary launch becomes a single-flight owned by the terminal service: a
caller finding a launch already in flight joins it and waits for its result
rather than starting its own, and `ResolvePrimary` returns the exec that launch
produced instead of re-scanning. Exactly one primary, one read-modify-write of
the launched flag, one prompt. This is also precisely the readiness wait tier 3
needs — joining an in-flight launch is a wait on a completion signal, not a
poll — and it closes the same race for `harnessMode: config`, where the launch
is deferred to the first attach and two concurrent attaches would otherwise
both launch.

**The launch outlives its caller.** It runs under a context detached from
whichever caller happened to start it, so an attach that times out or a client
that disconnects cannot abort an install that boot and other joiners are
waiting on. Each joiner waits under its own context and its own timeout. A
failed launch is reported to everyone joined to it and clears the latch, so a
later attach retries rather than inheriting a permanent failure.

### Timeouts nest, and measure stall rather than duration

Each tier has its own timeout, and each inner one is strictly shorter than the
tier above it. The innermost failure is therefore the one that surfaces: a
harness that cannot install reports as a harness that cannot install, not as a
generic control-plane deadline. The client's request context cuts every tier
short at once when the caller goes away. Tier 1 is 2 minutes; the tiers below
it take budgets that fit inside that.

**The budget is reset by observable progress.** A 2-minute cap on total
duration would be wrong, because the one thing that legitimately exceeds it is
an image pull: a multi-gigabyte pull over a slow link takes as long as it
takes, and killing it at the two-minute mark would abort work that was
proceeding perfectly well — repeatedly, since the next attach starts the pull
over. So the timeout is a stall timeout. Two minutes without progress is a
failure; progress refreshes the clock. This is what makes "it goes into
progress" a sufficient answer to "what if the pull is slow": progress is not
merely how the wait is *displayed*, it is how the wait is *bounded*.

### Progress is a frame, and only tier 1 sends it

Blocking for a provisioning-length wait on a silent socket would lose what
`disco run` prints today ("provisioning…", "Preparing harness…") and, far
worse, would leave a multi-minute image pull looking indistinguishable from a
hang. Progress is therefore streamed to the client on the attach connection as
a new frame type on the existing protocol (`execstream/frame`).

The frame is **opt-in per attach**, requested by the client the way `replay` is
today. It cannot be sent unconditionally: `execstream/client` fails an attach
on any frame type it does not know (`execstream/client/session.go:217`), so an
unrequested progress frame would break every already-installed CLI against an
upgraded server. Opting in keeps the wire byte-identical for clients that did
not ask, and the `default:` arm stays a genuine protocol error rather than
becoming a silent ignore.

**Observing progress and transmitting it are separate jobs.** Only the control
plane relays: it accepts the client's websocket itself, emits progress frames
while it waits, dials downstream once tier 1's own gate opens, and pipes bytes
from then on. The pool agent and sandbox agent keep the pass-through reverse
proxies they have today (`pool-agent/server/sandbox_proxy.go:154`,
`server/internal/server/sandbox_agent_terminals_proxy.go:68`) and emit no
frames at all. They report what they observe up the channel their observations
already travel on: the pool agent's push to
`/api/pools/{poolId}/sandbox-states` (ADR 0017 §10), which is where
container-lifecycle facts are already reported with boot-id and sequence
ordering.

Making every tier emit its own frames would mean turning both proxies into
websocket relays — per-hop framing awareness, an extra copy on the hot path,
per-hop keepalive, and `execstream/resume`'s reconnect logic spanning relays —
to move facts that already have a channel. One relay, at the tier that already
terminates the client's connection, and the tiers below report as they do now.

### Image pull progress

An image pull is the longest thing an attach can wait behind, and today it is
invisible: `ensureImageAvailable` calls `pull.Wait(ctx)`
(`pool-agent/sandboxruntime/runtime.go:503`), which drains the daemon's
progress stream and discards it. The moby client offers it as
`ImagePullResponse.JSONMessages`, layer by layer with `current`/`total` byte
counts.

The pull is not on the attach's own path — it happens in the pool agent's
sandbox *create*, driven by the reconciler after `POST /sandboxes`
(`pool-agent/sandboxruntime/runtime.go:406`), which is a different request from
the attach that later waits behind it. So the pool agent consumes the message
stream, aggregates it per sandbox (bytes done against bytes total, layers
complete against layers total — not per-layer chatter), and reports it upward
on the sandbox-states channel at a rate fit for a status line rather than at
the daemon's. The control plane records it on the sandbox as
`runtime.provisionProgress` and publishes it, so it is readable by anything
watching the sandbox; the attach turns it into progress frames, and it is what
refreshes the stall budget.

Progress rides the state channel but is **not** a state observation. It travels
in its own `progress` array on the report body, because a state observation
always carries an observed state and progress has none, and because `complete`'s
"every sandbox I host" claim is about states only — a progress report must never
be read as a sync that omits every other sandbox. For the same reason it is
applied separately and marks nothing dirty: a pull in flight is work proceeding,
not drift for the reconciler to repair.

This is the case that justifies the whole progress mechanism: a user who runs
`disco run` against a cold pool waits minutes on a pull, and the difference
between a progress line and a silent socket is the difference between a working
system and a hung one.

### Source delivery stays the client's, and stays exempt

A sandbox whose source must be pushed cannot start until the client pushes it,
so no server-side wait can subsume the step: the client waits for
`awaiting_source`, pushes, calls complete-source-push, and only then attaches.
This is the one place a poll remains, and it is bounded by the client's own
push rather than by provisioning.

## Alternatives rejected

**Keep the client polling (status quo).** Every client reimplements the loop,
and they diverge in practice — the launcher's create does not poll at all,
which is exactly why its attach fails on a fresh sandbox. Cost also scales
with provisioning time for information the server already knows the instant it
changes.

**One wait, in one place.** Whichever tier held it would have to poll the tier
below to see the readiness it cannot observe: the control plane cannot see a
container, the pool agent cannot see a primary terminal. Splitting the wait is
what keeps every level's check local and event-driven.

**One global timeout carried down the stack.** Simpler to state, worse to
diagnose: every failure surfaces as the same deadline regardless of which stage
stalled, and no tier can apply a budget appropriate to what it is waiting for.
Nested timeouts make the innermost cause the reported one.

**Make create synchronous — return only when the sandbox is attachable.**
Rejected because it breaks `--detach` and every non-interactive caller, and
because it makes the create request's duration hostage to provisioning. Attach
is the call that means "I want to use this now"; create is not.

**A plain mutex around `EnsurePrimary`.** It would stop the duplicate launch
and the double prompt, which is most of the problem. Rejected because the
second caller then re-runs the check-then-act and resolves by scanning `List()`
again, so it can attach to a different exec than the one it waited behind, and
because a queued caller that gives up leaves nothing behind for the next one.
Tier 3 needs a *result* to wait for — the launched exec, or the launch's
error — and a single-flight hands back exactly that.

**A server-side poller that pre-warms readiness.** Rejected for the reason
[ADR 0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md) records:
the control plane's only channel into a sandbox requires a user principal
behind the request, and a background ticker has none.

## Consequences

- `disco run` drops its two poll loops and the launcher gains the wait it never
  had; both reach one attach call. Consolidating the two create paths (the
  divergence this work was scoped from) gets materially smaller as a result.
- Attach becomes a long-lived request before it is a long-lived stream. The
  server's read and write deadlines around the websocket upgrade
  (`server/internal/server/server.go:121`) must admit a wait of tier-1 length.
- A caller that wants the old fail-fast answer needs a way to say so; waiting
  is the default because it is what every interactive caller wants.
- The progress frame is a protocol addition, negotiated per attach so the wire
  is unchanged for clients that do not request it. No existing frame changes
  meaning.
- The control plane's attach route stops being a reverse proxy and becomes a
  relay. That is the single largest piece of work here, and the place to look
  first when an attach misbehaves.
- Pull progress becomes a reported observation, so it is subject to the
  sandbox-states channel's existing ordering rules rather than being a live
  stream the client reads directly.
- `disco run` and the launcher both display progress, so the wait is legible
  without either of them polling for it.
