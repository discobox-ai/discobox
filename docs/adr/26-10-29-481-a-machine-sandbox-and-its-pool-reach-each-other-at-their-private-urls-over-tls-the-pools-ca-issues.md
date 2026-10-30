# 26-10-29-481 — A machine sandbox and its pool reach each other at their private URLs, over TLS the pool's CA issues, and the sandbox reaches nothing else

- **Status**: Accepted (§§2–3 amended 2026-10-30, before anything shipped
  against it: a sandbox reaches its pool by a name discobox resolves itself,
  and the agent's TLS is a pool's setting, on unless the pool disables it;
  narrows [26-10-27-345](26-10-27-345-a-machine-sandbox-is-made-again-in-place-and-its-bootstrap-names-where-its-agent-listens.md)
  §2's one URL the pool reaches a machine sandbox's agent at)
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

- **A direct path** (`PrivateURL != URL`): `https://discobox-pool-proxy:<port>`
  for the proxy, credentials, DNS and BuildKit, as on a Docker pool.
- **No path but the public URL** (`PrivateURL == URL`, an edge):
  `wss://<host>/tunnel/<service>`, the 719 tunnel, served at the pool's
  service, since that is all an edge reaches. Whether a build goes through it
  is 26-10-29-160's.

**The names are discobox's own.** A URL and the certificate it is verified
against name a private name discobox issues — `discobox-pool-proxy` for the
pool, and for each sandbox's agent a name under `sandbox.discobox.internal`
(§3) — never the provider's name for a machine or its address. Discobox keeps
its own answer for each, as a private DNS would: a sandbox's bootstrap carries
the provider's host its pool's name stands for (boxd's `<pool>.boxd`), which
its bridges dial in the name's place and resolve through the provider at each
dial, so an address that moves is followed. A Docker pool's name is its
container's alias on the pool's network, so its sandboxes are given no
answer and resolve it as they always have.

### 3. The sandbox agent serves TLS the pool's CA issues

The pool's mTLS CA, which already signs its proxy's server certificate and
every sandbox's client certificate, signs a server certificate for each
sandbox agent, for the sandbox's private name (`<sandbox ID, as a DNS
label>.sandbox.discobox.internal`, §2). The agent serves its API over TLS with
it, and the pool verifies the certificate against that CA and for the sandbox
it meant to reach, wherever it dialed: a container's address, a machine's
provider host. TLS says who answered; the tokens still say what a caller may
do.

- **On every runtime**, a Docker pool's sandboxes included.
- **A pool's setting, on by default.** A pool serves its sandboxes' agents over
  TLS unless it sets `disableAgentEndpointTls`; a pool made from now on does
  not, and an upgrade sets it on every pool made before, so a pool's sandboxes
  keep what they were made with. The pool agent is told it in its bootstrap,
  and tells each sandbox in the sandbox's bootstrap whether its agent serves
  TLS alone or plain HTTP alone: every box of a pool is the same, and the pool
  needs to learn nothing from an agent before it sends one anything. A pool
  reached through an edge (§2's second form) serves plain HTTP whatever it
  sets: the edge speaks HTTP to the agent, and the pool's only assurance is
  the edge's TLS and its tokens.
- **A sandbox is dialed as its bootstrap told it**, recorded with it, so a
  pool whose setting changes reaches the sandboxes it already made as before.
  A sandbox takes the setting when its box is made again: a machine at its
  next start, which places its bootstrap again, a container when it is
  replaced. Nothing does that for every sandbox when the setting changes; the
  one reason to change it is a pool made before this that is to serve TLS, and
  its sandboxes are restarted, or made again, by hand.
- **The keypair is placed before the agent first starts**, beside the
  sandbox's bootstrap, and again at every start: a machine's place step, a
  container's config volume, through a root that does not follow what the
  sandbox planted there. It is renewed in the runtime-config document as a
  client certificate is. Like the client key it is never kept in an exported
  tree.

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
  (767's wording). A hosts file holds an address, fixed when it is written, in
  a guest whose root can change it; the bootstrap's answer is the provider's
  host, resolved at each dial.
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
- **The agent says it serves TLS, and the pool dials TLS once it has**, as
  `controlPlanePower` is learned (as this section first said, before anything
  shipped against it). Every path that delivers a document before the status
  poll — a boot, a resume, settling sources — would deliver its first one,
  with the sandbox's client key, in plain HTTP, and a peer answering at the
  sandbox's address first could hold the pool to plain HTTP. A setting the
  pool owns needs nothing learned.
- **A verified TLS probe before the first delivery**, recorded for the agent
  layer. It would learn without the status, and still be held to plain HTTP
  by a peer that answers the probe with something else.
- **The pool's address, or the provider's name, in the endpoints and
  certificates.** An address fixed in a sandbox's bootstrap at create is
  wrong once the pool's machine moves, and a provider's name is the
  provider's: the names discobox verifies are its own.

## Consequences

- disco-vm is bumped to a version with ADR 26-10-29-129.
- The pool's bootstrap gains the pool machine's private URL; the machine
  runtime dials a sandbox's private URL instead of its public one, and both
  runtimes dial an agent over TLS where their pool serves it.
- The sandbox agent serves TLS or plain HTTP, as its bootstrap says, with
  certificate material the pool issues, places and renews.
- Pools gain `disableAgentEndpointTls`, and an upgrade sets it on every pool
  that exists. A pool's bootstrap moves with it, so a pool whose setting
  changes is placed again and its agent restarted, a Docker pool's as a
  machine pool's. So does every pool once on the upgrade itself: its
  bootstrap now names the setting, and a Docker pool's record of what it
  placed held no placement to compare against. The release that brings it
  moves the pool's bundles too, which restarts the agent and proxy anyway;
  running sandboxes are not restarted.
- A sandbox pinned to an agent that predates this, in a pool that serves TLS,
  is unreachable: it ignores the bootstrap's word. A pool made from now on
  pins none; a pool made before is the one that would, and it keeps plain
  HTTP until its setting is changed.
- A machine pool behind an edge serves the 719 tunnel at its service.
- 551 §5's precondition is met, and machine pools schedule.

## Deferred

- **The server → pool agent hop stays as the driver gives it**: boxd's edge
  and its certificate, a Docker pool's local listener. The control plane
  cannot verify a pool-issued certificate through an edge. Revisit when a
  provider's public URL is a direct path, or when the control plane pins each
  pool's CA at registration.
