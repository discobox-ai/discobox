# 26-10-29-481 — A machine sandbox and its pool reach each other at their private URLs, over TLS the pool's CA issues, and the sandbox reaches nothing else

- **Status**: Proposed
- **Date**: 2026-10-29
- **Relates to**:
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §§5–7 (reachability is the backend's; no egress but the proxy;
  authenticated paths to pool services),
  [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
  §§4–5 (sandbox machines isolated on the pool's network; the hop and egress
  a precondition of the first),
  [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
  (the WebSocket tunnel through an edge),
  [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md)
  §2 (the pool reaches a machine sandbox at its one URL),
  [26-10-27-767](26-10-27-767-a-machine-pool-runs-what-every-pool-runs.md)
  (a sandbox machine reaching its pool by name is #129's hop),
  [26-10-29-160](26-10-29-160-a-sandbox-reaches-its-pools-build-registry-at-a-host-its-pool-proxy-answers.md)
  (the build registry behind the proxy),
  disco-vm ADR 26-10-29-129 (an instance's public and private URLs, and its
  egress fence).

## Context

A machine pool schedules nothing: until a sandbox machine's only way out is
its pool's proxy, a sandbox could reach the network around the proxy's policy
and audit (551 §5, issue #129). The owner's decision on #129 (2026-10-24):
every VM is launched with no egress but its pool's proxy and pool agent, which
run on the pool's own machine; the hop is a network path to that machine, set
by the pool agent at create, and the proxy stays the only way out.

A pool is the same thing on every runtime. Its agent and its proxy unit run on
the pool's box — a Docker pool's container, a machine pool's machine — and its
sandboxes reach the proxy, the credentials endpoint, DNS and the BuildKit
mediator there over mTLS with their pool-issued client certificates
(0126 §7): `https://discobox-pool-proxy:<port>`, verified as
`discobox-pool-proxy`, or the 719 tunnel through an edge. A machine pool
already hands its sandboxes those endpoints. What it lacks is the network
under them:

- **Nothing names the pool's machine.** In a Docker pool
  `discobox-pool-proxy` is the pool container's alias on the pool's network;
  no sandbox machine resolves it.
- **The pool reaches a sandbox through boxd's edge** (345 §2), its public URL,
  which terminates TLS with boxd's certificate: the traffic leaves the
  provider's network and comes back, and nothing in it says which sandbox
  answered.
- **Nothing fences a sandbox machine.** `isolated` strips a boxd machine of
  the org's credentials and peers, not its internet.

Both agents also serve plain HTTP. The pool agent and the sandbox agent each
authenticate every request by a token, so a call is authorized; but on a
network every sandbox of the pool shares, an address says nothing about who
answers it, and the pool delivers a sandbox its runtime-config document —
client key, secrets and sentinels — to whatever answers at that address.

disco-vm ADR 26-10-29-129 gives every instance a public URL and a private one,
where its peers reach it, and lets a driver fence an instance's egress to its
networks.

## Decision

### 1. Three hops, two URLs, on every runtime

- **The server reaches the pool agent at the pool's public URL**, as today.
- **The pool reaches a sandbox's agent at the sandbox's private URL.**
- **A sandbox reaches its pool's services at the pool's private URL.**

A Docker pool already works this way: a container's address on the pool's
network is its private URL. A machine pool learns both from its driver: the
server writes the pool machine's private URL into the pool's bootstrap beside
its listen address, and the pool asks its own engine for each sandbox
machine's. A driver whose `PrivateURL` is empty cannot carry a machine pool.

### 2. The endpoint's scheme says the transport

The pool derives its sandboxes' endpoints from its private URL, and they say
which transport by scheme, as 719 made them:

- **A direct path** (`PrivateURL != URL`): `https://<address>:<port>` for the
  proxy, credentials, DNS and BuildKit, verified as `discobox-pool-proxy` by
  the endpoints' `ServerName`. The address is an IP literal: the pool agent
  resolves its private URL's host on its own machine, so a sandbox needs no
  name and no DNS to reach its pool.
- **No path but the public URL** (`PrivateURL == URL`, an edge):
  `wss://<host>/tunnel/<service>`, the 719 tunnel, served at the pool's
  service, since that is all an edge reaches. Whether a build goes through it
  is 26-10-29-160's.

### 3. The sandbox agent serves TLS the pool's CA issues

The pool's mTLS CA, which already signs its proxy's server certificate and
every sandbox's client certificate, signs a server certificate for each
sandbox agent, naming the sandbox. The agent serves its API over TLS with it,
and the pool verifies the certificate against that CA and for the sandbox it
meant to reach. TLS says who answered; the tokens still say what a caller may
do.

- **On every runtime**, a Docker pool's sandboxes included.
- **The keypair is placed before the agent first starts**, with the sandbox's
  bootstrap: a machine's place step, a container's config. It is renewed in
  the runtime-config document as a client certificate is, and like the client
  key it is delivered and never kept in an exported tree.
- **Behind an edge** (§2's second form), the edge speaks HTTP to the agent, so
  the agent serves plain HTTP there, as the bootstrap says, and the pool's
  only assurance is the edge's TLS and its tokens.

### 4. Every sandbox machine is fenced to its pool at create

The pool agent creates every sandbox machine isolated, on the pool's network
alone, and with its egress fenced through disco-vm (`CreateOptions.Egress`):
nothing beyond that network on a direct path, the pool's public host alone
behind an edge. The driver sets the fence and reads it back before Create
returns, so it holds before anything is placed, on a fresh machine and on a
restore. The pool's own machine keeps its internet: its proxy is the sandboxes'
way out.

A machine pool schedules once its driver lists `Isolation` and `Egress` and
gives the pool a private URL; the condition that keeps it unschedulable goes.

## Alternatives rejected

- **Name the pool `discobox-pool-proxy` in each sandbox machine's hosts file**
  (767's wording). It needs the same address, as one more file placed in a
  guest whose root can change it; `ServerName` already keeps the certificate's
  name apart from the address dialed.
- **Always the tunnel.** One endpoint form, but every byte through WebSocket
  framing and a splice where the machines already have a direct path.
- **Keep reaching a sandbox at its public URL.** The pool's traffic to its own
  sandboxes leaves the provider's network, the pool's machine needs the
  internet to reach them, and boxd's certificate says nothing about which
  sandbox answered.
- **Plain HTTP on the private network, as a Docker pool has it.** Every
  sandbox of the pool is on that network, so one that came to answer at a
  peer's address would be sent the peer's runtime-config document.
- **A certificate authority of the control plane's for the agents.** The pool
  already holds one its sandboxes trust, renews from it without a restart, and
  is the only party that dials a sandbox's agent.
- **`isolated` alone as the fence.** It leaves a boxd machine's internet on.

## Consequences

- disco-vm is bumped to a version with ADR 26-10-29-129.
- The pool's bootstrap gains the pool machine's private URL; the machine
  runtime dials a sandbox's private URL instead of its public one, and the
  Docker runtime dials its containers over TLS.
- The sandbox agent gains a TLS listener and its certificate material; the
  pool issues, places and renews it.
- A sandbox whose agent predates this serves plain HTTP. The pool records
  which a sandbox's bootstrap asked for and dials that, so it reaches the
  sandbox until its pins move.
- A machine pool behind an edge serves the 719 tunnel at its service.
- 551 §5's precondition is met, and machine pools schedule.

## Deferred

- **The server → pool agent hop stays as the driver gives it**: boxd's edge
  and its certificate, a Docker pool's local listener. The control plane
  cannot verify a pool-issued certificate through an edge. Revisit when a
  provider's public URL is a direct path, or when the control plane pins each
  pool's CA at registration.
