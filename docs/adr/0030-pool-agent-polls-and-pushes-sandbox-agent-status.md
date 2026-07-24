# 0030 — Pool-agent polls sandbox-agent status and pushes it to the control plane

- **Status**: Accepted
- **Date**: 2026-07-24
- **Relates to**: [ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §10, a separate pool-agent → control-plane reporting channel for observed
  container *power* state (running/stopped/...). This ADR's channel carries
  sandbox-agent-computed git/session/connection status instead — a different
  kind of fact, observed by a different component, and deliberately kept on
  its own endpoint rather than folded into §10's batch (see Alternatives
  rejected).

## Context

Nothing today observes, on a running sandbox: whether its mounted git sources
are clean or dirty, whether the coding-agent harness inside it (claude-code,
codex-cli, opencode) is actively working, idle, or blocked waiting on the
user, or how many terminal/exec clients are attached to it.
`Sandbox.RuntimeState` is provider-owned, container/VM-lifecycle-scoped state
(`server/internal/sandbox.Provider.Get`/`List`); `Sandbox.LastActiveAt` is
bumped only on lifecycle transitions (start/stop), never from live in-sandbox
activity. Neither can answer "is anyone using this sandbox right now," which
is exactly what a future idle-timeout feature needs.

The obvious shape — sandbox-agent exposes a status endpoint, discobox-server
polls it on a ticker — runs straight into a decision this project already
made. [ADR 0002](0002-harness-config-is-the-only-harness-concept.md)
considered and rejected exactly this shape for a different feature
(watching a configure terminal to completion):

> Appealing because it is self-healing: a reconciler re-derives state every
> tick, so no event can be missed and no client can fail to report. Rejected
> because it forces the control plane to originate requests to a sandbox with
> no user principal behind them, which requires a lease acquisition that skips
> scope authorization — a permanent hole kept shut only by convention. It also
> cuts against the established direction, where agents report *to* the control
> plane (`/api/workers/{workerId}/sandbox-removed`), not the reverse.

That reasoning is architectural, not feature-specific: discobox-server's only
channel into a running sandbox,
`Service.AcquireSandboxHTTPClient` → `authorizeRequestedScopes`
(`server/internal/resources/sandboxes/service.go:322,357-371`), hard-requires
an inbound request carrying a `PrincipalTypeUser`. A background ticker has no
user behind it. Bypassing that check for this feature would reopen the exact
hole ADR 0002 closed, for a different reason each time a new background-poll
feature comes along.

`sandbox-agent/DESIGN.md`'s Boundary Rules independently forbid the opposite
direction: *"Do not call back to the pool host-harness or server; resolved
config is injected into the sandbox and read locally."* Sandbox-agent must
only ever answer inbound authenticated requests — it cannot push either.

Two components already sit between these two facts with the right shape:

- Pool-agent already reaches into its own hosted sandboxes directly and
  without a user principal — `waitForSandboxAgent`
  (`pool-agent/sandboxruntime/runtime.go:1225-1269`) resolves a sandbox's
  container IP (`HTTPBaseURL`) and calls its `/healthz` today, as a one-shot
  readiness check on create/start.
- Pool-agent already reports to discobox-server as a pool-level principal,
  no user involved — `UpdatePoolStatus` (`pool-agent/http.go`, on a standing
  interval) and `ReportSandboxStates` (`pool-agent/http.go`, ADR 0017 §10,
  driven by Docker events plus a periodic complete sync) both sign a pool
  assertion and POST it; the server verifies `PrincipalTypePool`, never a
  user.

Extending this same shape into a second standing, periodic loop — for a
different kind of observation — closes the gap without reopening either
boundary.

One auth problem remained once that shape was chosen: pool-agent does not
hold a key sandbox-agent trusts. Sandbox-agent's `SignedTokenAuthenticator`
(`sandbox-agent/server/auth.go:60-82`) validates against exactly one public
key, the control plane's own — the private half is sealed in the server's DB
and only `poolagentauth.Manager` (`server/internal/auth/poolagent/auth.go`)
can sign with it, via `CreateSandboxAgentToken`
(`auth.go:105-110`), currently called only from a live user request
(`server/providers/poolruntime/agent_client.go:199-227`) and handed to
pool-agent purely as a header to relay
(`pool-agent/server/sandbox_proxy.go:74,162-164`). Pool-agent is a relay of a
token minted elsewhere today, not a minter.

## Decision

**Pool-agent polls each sandbox-agent it hosts on a standing local interval,
and pushes an aggregated batch of results to discobox-server on the same
cadence. Discobox-server never originates a call into a sandbox for this
feature.** Concretely, three pieces:

1. **Pool-agent → sandbox-agent: pull.** A new standing goroutine (not the
   existing one-shot `waitForSandboxAgent`) polls every currently-running
   hosted sandbox's new authenticated status endpoint on an interval (15–20s),
   isolated per sandbox so one unreachable sandbox never blocks the batch.
   This is a genuinely new responsibility for pool-agent — a standing loop,
   not a one-shot check — and should be reviewed as such.

2. **Pool-agent → discobox-server: push.** Once per poll cycle, pool-agent
   POSTs everything it successfully collected that tick to a new endpoint,
   authenticated with the same pool assertion `UpdatePoolStatus` already uses.
   Sandboxes that failed to poll this tick are omitted from the batch rather
   than pushed as stale — a gap in this tick's data is preferable to
   overwriting a good prior value with nothing.

3. **A new, deliberately narrow token-minting endpoint closes the auth gap.**
   Discobox-server gains an endpoint pool-agent calls (authenticated as
   itself, `PrincipalTypePool`, no user principal needed) to fetch short-lived
   sandbox-agent tokens for the sandboxes it hosts, cached and refreshed
   before the existing 15-minute TTL expires — the same refresh-loop shape
   pool-agent already runs for its resolve-secrets token
   (`pool-agent/agent.go:100-130`). **The endpoint accepts no
   caller-supplied scope.** It always calls
   `poolagentauth.Manager.CreateSandboxAgentToken` with a scope list hardcoded
   in the handler to exactly `["status:read"]`. There is no code path — buggy
   or malicious — by which this endpoint can mint anything broader; the scope
   is a Go literal, never derived from request input. Sandbox-agent's
   existing scope model (`ScopeTerminalRead`/`ScopeExecRead`, etc.,
   `sandbox-agent/server/auth.go:16-22`) gains one sibling, `status:read`,
   which authorizes nothing but the new status route.

This keeps every existing trust boundary exactly where it is: sandbox-agent
still only answers inbound authenticated requests; discobox-server still
never originates a sandbox-scoped request without a user (or, now,
a pool) principal behind it; and the one new credential this introduces is
capped, by construction, to read-only status.

## Alternatives rejected

**Discobox-server polls sandbox-agent directly on a background ticker.**
This is ADR 0002's rejected alternative verbatim, in a new feature's
clothing. `authorizeRequestedScopes` requires `PrincipalTypeUser`; a ticker
goroutine has no user context to offer it. This ADR reconfirms that reasoning
in a new context — it does not revisit or weaken it.

**Discobox-server polls pool-agent instead of sandbox-agent.** Moving the
poll target up one hop does not change who originates the request: a
server-side ticker still runs with no user principal, so it still cannot
legitimately pass `authorizeRequestedScopes`, and inventing a bypass for
"server calling pool-agent" is exactly as much of a hole as inventing one for
"server calling sandbox-agent."

**Sandbox-agent pushes status to pool-agent or to discobox-server on its own
initiative** (e.g. on a timer, or piggybacked on hook events it already
receives). Rejected outright: it violates `sandbox-agent/DESIGN.md`'s
Boundary Rule that sandbox-agent must never call back to the pool host or
server. That rule exists so a compromised or buggy sandbox cannot originate
outbound traffic toward trusted infrastructure; a status-reporting exception
would be a hole with the same shape as the one this feature is designed
around, just moved to the other side of it.

**Pool-agent signs its own status-poll tokens locally**, with sandbox-agent
extended to trust a second issuer (the pool's own already-existing keypair,
distributed via the sandbox manifest). This was seriously considered — it
fully decouples the poll loop from server availability, since pool-agent
already holds this key for other purposes. Rejected in favor of server-minted,
cached tokens because it is a materially bigger change to sandbox-agent's
trust surface: a second cryptographic issuer, plus scope-ceiling-by-issuer
logic to guarantee a pool-signed token can never claim a broader scope than a
control-plane-signed one, needs to be introduced and gotten right. The
benefit it buys — no server round-trip, ever — is small in practice, since
the existing token TTL (15 minutes) already gives a refresh loop minutes of
slack against transient server unavailability. Smaller, better-understood
trust surface won over full decoupling.

**Generation-guarded writes (`WithGeneration`) for the new status fields**,
matching how desired-state fields on `Sandbox` are written
(`server/internal/store/sandboxes.go:69-73,178-211`). Rejected: agent-reported
telemetry is not part of the desired/observed-generation contract
`ResourceLifecycle` governs. Gating a benign polling write on a generation
match would make it spuriously fail whenever unrelated desired-state
reconciliation touches the same row at the same moment, for no correctness
benefit — the two kinds of writes don't conflict on any field that matters.

**Fold this into ADR 0017 §10's sandbox-state report** rather than adding a
second channel. Tempting: both are "pool-agent observes and pushes," on
similar cadences, to the same server. Rejected because they are different
observers of different facts with different failure semantics. §10's states
come from pool-agent itself watching Docker events — cheap, always available,
no dependency on anything inside the sandbox. This feature's status comes
from *asking sandbox-agent*, which requires reaching an authenticated route
inside a container that may be slow, wedged, or (during a harness crash)
simply not answering — none of which should make §10's power-state report
late or fail, since that report is load-bearing for existence reconciliation
(ADR 0017 §13) in a way this feature's telemetry is not. One failing sandbox
already must not block the batch (Decision, item 1); a shared channel would
make that isolation a property of every future consumer of §10's batch to
maintain, rather than a property of this feature alone.

**Building this on `server/internal/reconcile`** (the engine already used for
pool and sandbox desired-state reconciliation). Rejected: that engine is a
mark-and-sweep *converger* — a `Reconciler` is expected to reach a terminal
state and stop being re-marked dirty. This feature has no desired state to
converge toward; it is a plain "store what pool-agent reported" write on
every push, which doesn't fit the dirty-set model and would be forcing an
abstraction where a handler and a store method suffice.

## Scope

Full hook-event-driven session state (running / idle / needs-input / exited /
failed) is built for **claude-code** only, since it is the tier-1 harness.
Other harnesses (codex-cli, opencode) get a generic fallback derived purely
from the underlying exec process's liveness (running while the process is
alive, exited/failed otherwise) — correct but coarse. This is added as a new
optional harness capability (`harness.SessionStateDeriver`, sibling to the
existing `Converser` capability, `harness/driver.go:147-149`), not a change to
the required `harness.Driver` interface, so harnesses without real session
telemetry are not forced to implement anything new.

## Non-goals

This ADR does not add a live, on-demand proxy read of sandbox-agent status
through discobox-server (the way `list-harness-hooks` proxies today) — only
the periodically-pushed, persisted copy on the `Sandbox` row. A live read-through
path, if ever wanted, is a separate decision with its own authorization shape
to work out (most likely: a user-context call via the existing
`AcquireSandboxHTTPClient`, which is already fine for that case precisely
because it *does* have a user principal).

This ADR also does not build idle-timeout enforcement — only the persisted
shape (state plus the time it began) that such a feature would need.

## Consequences

- Pool-agent gains a standing background responsibility (a status poll loop)
  it did not have before, and a small amount of independent state (cached
  per-sandbox tokens). Both must respect the same "read-only, never affects
  lifecycle" discipline as the rest of pool-agent's telemetry paths — a status
  poll failure must never be treated as a signal to stop or recreate a
  sandbox.
- Discobox-server's pool-agent-facing surface grows two endpoints: the
  scoped token mint and the status push/ingest. Both are pool-principal-only,
  matching the existing `UpdatePoolStatus` shape.
- Sandbox-agent gains one new scope (`status:read`) and one new authenticated
  route; no change to its Boundary Rules, since it still only answers inbound
  requests.
- Status data can be as stale as one poll interval (15–20s) plus push latency.
  This is acceptable for the stated use cases (session visibility, future
  idle-timeout) and is explicitly not meant to back a low-latency UI.
- A sandbox in a pool that is itself unreachable from the control plane
  (network partition) stops reporting status but keeps running — this is a
  gap in observability, not a lifecycle event, and nothing here changes that.

## Deferred

- **Live/on-demand status read-through** via `AcquireSandboxHTTPClient`, for a
  UI that wants fresher-than-poll-interval data on demand from a real user
  request. Revisit if the persisted, polled copy proves too stale for a
  concrete feature.
- **Per-harness session-state deriving for codex-cli and opencode.** Revisit
  once claude-code's deriver has run in practice; codex-cli's smaller hook
  event set (`SessionStart`/`UserPromptSubmit`/`Stop`, no `SessionEnd`/
  `Notification`) would need its own mapping, and opencode has no fixed hook
  vocabulary to map at all today.
- **Idle-timeout policy itself.** This ADR only makes the state and its
  timestamp available; deciding what to do with them is separate work.
