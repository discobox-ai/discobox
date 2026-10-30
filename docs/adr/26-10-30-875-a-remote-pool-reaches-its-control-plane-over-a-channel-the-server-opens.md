# 26-10-30-875 — A remote pool reaches its control plane over a channel the server opens to it

- **Status**: Proposed (would supersede
  [26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
  §2's and §4's pool `apply` fetching its bundles from the asset routes, and
  §3's pool `apply` as a client of them)
- **Date**: 2026-10-30
- **Relates to**:
  [0052](0052-iroh-is-an-optional-endpoint-scheme.md) §5 (nothing
  unauthenticated reaches the handler surface),
  [0095](0095-an-enrolled-iroh-id-is-a-managed-resource.md) §1 (what reaches
  the carrier hub is the default user),
  [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)
  (a sandbox's API calls go through its pool),
  [26-10-29-481](26-10-29-481-a-machine-sandbox-and-its-pool-reach-each-other-at-their-private-urls-over-tls-the-pools-ca-issues.md)
  (a machine sandbox and its pool reach each other privately)
- **Issue**: #397, part of #42

## Context

Three providers make a pool on a machine somewhere else: `digitalocean`,
`execvm`, and `vm` on a remote driver (boxd). Each requires `controlPlaneUrl`
(`poolruntime.RequireControlPlaneURL`; `discovm.validateFor` also requires
`http(s)`), because the pool dials its control plane. A server on a laptop
behind NAT cannot run any of them without a public URL or a tunnel its
operator sets up.

Of a remote pool's hops, only that one goes toward the server:

| Hop | digitalocean, execvm | vm on boxd |
| --- | --- | --- |
| server → pool agent | the host's published agent port, plain HTTP on digitalocean (whatever URL its command answers on execvm); Docker over SSH | the pool machine's HTTPS URL |
| sandbox ↔ pool | containers on the pool's own Docker host | the provider's private network (ADR 26-10-29-481) |
| sandbox → discobox API | through its pool's gate (ADR 0140), so through the hop below | same |
| **pool → control plane** | `controlPlaneUrl`, dialed with `pkg/wire` | same |

That hop has three dialers:

- the pool agent, signing every call with the pool's key: status reports, the
  asset API, everything `HTTPClient` sends;
- the proxy unit, a separate process of the same binary, carrying a bearer
  token the agent mints for it and writes to the resolve-context file
  (`startResolveTokenRefresher`): secret resolution, gateway headers, and the
  discobox-API gate;
- `discobox-provision apply`, the sandbox agent binary run in the pool's box
  before the agent exists, which fetches the pool's bundles from the asset
  routes as the pool (ADR 26-10-24-953 §§2–4).

The server already makes the opposite hop to every remote pool, and the
codebase already turns one around: a wslc guest cannot dial its host, so the
server holds a `cpmux` (yamux) session into it, the guest's
`discobox-cp-relay` serves `unix:///run/discobox/cp.sock` and opens a stream
per connection, and the server hands each stream to `carrierhub.Hub`, a
`net.Listener` the ordinary router serves. The agent dials `unix://` with
`pkg/wire` and knows nothing of it.

The owner asked for this hop to go over iroh (issue #397). That is weighed
below and rejected in favour of turning the hop around, as wslc does.

## Decision

### 1. The server holds a control-plane channel open to every pool that has no `controlPlaneUrl`

For a pool of `digitalocean`, `execvm` or `vm` on a remote driver with no
`controlPlaneUrl`, the server opens one long-lived connection to the pool agent,
at the same base URL every other server-to-agent call uses, as an HTTP upgrade
on the agent's API (`/control-plane-channel`). The request is a server request
like any other, signed with the server's pool-agent issuer key, under a scope of
its own; the agent refuses an upgrade it cannot verify. Over the upgraded
connection the two ends run TLS 1.3, and the `cpmux` session inside it, the
server as the client of both.

That TLS is the channel's own, whatever the agent's front is, and it is mutual
and pinned to keys both ends already hold: the agent presents a certificate for
the pool's enrolled key, which the server checks against the pool's record, and
the server presents one for its pool-agent issuer key, which the agent checks
against the `ControlPlanePublicKey` it already verifies server requests with.
No CA is involved. It is needed because the front is not always TLS — a
droplet's agent port is plain HTTP on a public address — and what the channel
carries is the pool's most sensitive traffic: resolved secrets, gateway
headers, broker answers, and the discobox API responses its sandboxes are
forwarded. A signed upgrade proves who asked for the channel; only the inner
TLS keeps what it carries from whoever is on the path. It is the shape ADR
26-10-23-719 already gives a sandbox's tunnel: the edge's transport outside,
the two ends' own mutual TLS inside.

The server keeps one channel per such pool from when the pool's box is made
until it is removed, redialing with backoff when it drops. A newer channel
replaces an older one on both ends; there is one server per data directory,
so there is never a second server to share it with.

### 2. The pool agent serves the channel to its own processes as a socket

The agent accepts the upgrade and serves a Unix socket on the pool's filesystem,
the way `discobox-cp-relay` does in a wslc guest: each connection to it is a
stream it opens over the session, naming `cpmux.TargetControlPlane`. The pool's
bootstrap names that socket as its control-plane URL (`unix://`), so the agent
itself and the proxy unit dial it with `pkg/wire`, unchanged. The proxy unit
keeps its agent-minted token; nothing on the pool learns a new transport or a
new credential.

The channel route is served before the agent's first status report, which today
precedes `Serve`. The first report waits a bounded time for a channel, and fails
the boot as an unreachable control plane does when none arrives.

### 3. A carrier stream is its pool's, and reaches only that pool's routes

Today a stream pushed to the carrier hub is served as any connection is, and a
request on it that no pool or sandbox credential claims falls through to
`DefaultUserAuthenticator`: the operator, with `ScopeAll` (ADR 0095 §1 names
this for a pool guest dialing the hub). On a pool machine that runs other
people's code, that is not acceptable.

`Hub.Push` takes the pool the stream belongs to, and the server's `ConnContext`
tags the connection with it. On a tagged connection:

- `PoolAuthenticator` and `SandboxForwardAuthenticator` apply as on any
  listener, and additionally require that the pool their credential names is
  the connection's pool.
- A new authenticator, before `DiscobotAuthenticator`, refuses with 401 any
  request on a tagged connection that neither claimed. `IrohPeerAuthenticator`
  and `DefaultUserAuthenticator` are never reached from one.
- `IsPublicPath` routes still answer, as they do everywhere.

This applies to every carrier stream, wslc's and vz's included: they are pool
agents too, and nothing they send needs more than its pool's routes. The
implementation proves that against those pools before it lands. It is a rule
about the hub, not about every pool: a libkrun pool reaches the server over
vsock and a local Docker pool over the server's mounted socket, neither through
the hub, and both keep the reach they have today.

### 4. `controlPlaneUrl` becomes optional for remote providers

For `digitalocean`, `execvm` and `vm` on a remote driver, `controlPlaneUrl` is
optional. Left empty, the pool uses the channel; given, the pool dials it as it
does today and no channel is opened. `discovm.validateFor` and
`RequireControlPlaneURL` stop requiring it. Nothing about the server's own
listeners is consulted: the channel needs the server to reach the pool, which
these providers already require, and nothing else. Local providers (Docker,
libkrun, vz, wslc) are unchanged.

Whether a pool uses the channel is part of its placement, so the bootstrap's
URL is the socket or the configured URL, and changing the setting re-provisions
the pool as any other change to its placement does.

### 5. A pool's maker places its bundles in the box, and `provision apply` dials nothing

`apply` runs before the agent, so before there is a channel. The server — the
pool's maker, already copying the agent tree into the box — also reads each
pinned pool bundle from its asset service and places it in the box's bundle
store with the same copy, and `apply` applies what is placed. It no longer
takes `--control-plane`.

This is new work for the server: today it opens only the agent bundle and
writes the pins as names and digests. It is done for every pool rather than
only channel pools, so provisioning has one path and never depends on how the
pool reaches its control plane. It supersedes ADR 26-10-24-953 where §§2–4 have
a pool's `apply` fetch its bundles from the asset routes; the pool agent's own
cache still fetches its sandboxes' bundles over those routes, through the
channel.

### 6. What is unaffected

- **Sandboxes.** Nothing a sandbox is given names the control plane. It reaches
  its pool at the endpoints its bootstrap names, and the discobox API through
  its pool's gate, whose upstream is the proxy unit's dial in §2.
- **The server-to-agent hop.** Still the provider's URL; the channel is one more
  request on it.
- **Relays and discovery.** None are involved: the channel is a connection the
  server opens to an address it already reaches.
- **`pkg/wire` and `pkg/endpoint`.** Neither changes scheme or role (ADR 0052
  §2): the pool dials `unix://` with `wire`, and iroh stays the client-to-server
  hop's.

## Alternatives rejected

**The pool dials the server's `discobox://` address over iroh**, as the issue
first asked. The pool agent would use `pkg/endpoint` for this hop, and the
server's iroh gate would admit the pool's enrolled key. Rejected for what it
costs against what it buys:

- **A new identity at the iroh gate.** An admitted iroh identity is the
  operator: `IrohPeerAuthenticator` re-runs admission per request and answers
  with the default user and `ScopeAll`. Admitting pool keys means changing
  `endpoint.IrohConfig.Authorize` to say *which kind* of identity it admitted,
  and every check downstream of it, and a mistake fails open into the
  operator's API.
- **The proxy unit has no key.** It would need its own iroh endpoint and a
  `DBXPEER` certificate (ADR 0104) the agent issues and renews for it, or its
  traffic routed through the agent, which is this decision's socket anyway.
- **A dynamic pool agent.** iroh-go loads a ~14 MB native library through
  purego, which makes the statically built pool agent (and the proxy unit and
  build mediator it also runs) a dynamic executable that needs glibc on every
  pool box and a writable cache for the library.
- **Relays and discovery.** A pool would depend on n0's relays or the
  deployment's, and on n0's discovery service, and the server would have to
  listen on iroh at all.
- What it buys over the channel is a pool that can reach a server the server
  cannot reach back — but such a pool cannot be run, since every operation on
  it is the server calling the agent.

**`pkg/wire` gains the `discobox://` scheme.** The iroh option with the least
code, and the worst of it: `wire` is built into the sandbox agent and the proxy
bridge, so every sandbox would carry the iroh library for a hop it never makes.

**Require an HTTPS front for a channel pool, or refuse the channel on a plain
HTTP agent URL.** Either keeps the channel's traffic off a plaintext path
without a TLS of its own. Rejected because it makes the agent's front carry the
pool's security: a droplet would need a certificate, a name and its renewal
before it could run without a public server, which is the setup this ADR
exists to remove, and an HTTPS front that terminates TLS outside the agent
(boxd's edge) would still see the traffic in the clear. Pinned TLS inside the
channel is end to end on every front.

**A tunnel the operator sets up.** Works today and needs nothing here. It is
what the issue exists to remove.

**Keep carrier streams at today's reach.** The wslc and vz guests are on the
operator's own machine, and ADR 0095 §1 accepted that they reach the operator's
API. A remote pool is not, and two kinds of carrier stream with different reach
would be a distinction the hub does not otherwise need. One rule for every
carrier stream is simpler and narrower.

## Consequences

- A server with no public URL, and no iroh listener, can run a remote pool. The
  only reach it needs is the one it already has: to the pool agent.
- The server holds one session per channel pool. While it is down the pool
  cannot report, resolve secrets or forward its sandboxes' API calls, and goes
  stale like a pool that lost its network. A pool that keeps working while the
  server cannot reach it is no longer possible for these pools; it was never
  useful.
- The agent's HTTP front (boxd's HTTPS URL, a droplet's agent port) carries a
  long-lived upgraded connection. A front that cuts idle connections breaks the
  channel; yamux's keepalive keeps it busy, and a cut one is redialed.
- Every carrier stream, wslc's and vz's included, loses the default user. That
  is a narrowing of what those pools can reach, and the change that lands it
  must show they never used it.
- The channel is end-to-end encrypted on every provider, which a plain HTTP
  agent port, today used for every server-to-agent call on digitalocean, is
  not. Those other calls are unchanged here.
- A pool's provisioning copies its bundles from the server instead of the box
  fetching them: the same bytes, on the maker's connection.
- `server/providers`, `pool-agent` and `server/internal/auth` `DESIGN.md`
  describe the new state when it lands; "Listen endpoints" no longer calls
  `controlPlaneUrl` required at config time for these providers.

## Deferred

- **A pool that dials out when it can.** A pool given a `controlPlaneUrl` dials
  it, and one without uses the channel; there is no fallback from one to the
  other. Revisit if a deployment needs both.
