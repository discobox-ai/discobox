# 26-10-08-561 — A sandbox reaches its origins at a host its pool proxy answers

- **Status**: Accepted (refines [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §4: says where the pool serves a sandbox its origins, and how a fetch reaches
  that route)
- **Date**: 2026-10-08
- **Relates to**: [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md).

## Context

ADR 0126 §4 ends the origin bind: a sandbox clones and fetches its sources over
authenticated Git HTTP served by its pool, and its `origin` remote is that URL.
The pool agent already serves the route (`.../sandboxes/{id}/git-origins/{slug}.git`),
accepting a token it issues the sandbox with scope `origin:fetch`, and the
sandbox agent clones from whatever URL its runtime-config document names.

Nothing said how a request from inside a sandbox reaches that route. The pool
agent's API listens where its backend puts it: TCP on a Docker pool, which
happens to be reachable from the sandbox network, and VSOCK alone on libkrun
and vz pools, which a sandbox cannot dial at all. ADR 0126 §7 also requires
that a sandbox reach pool services over a connection carrying its pool-issued
client certificate, through sandbox-local bridges that keep the key away from
ordinary processes — and an origin is fetched by ordinary processes: the
agent's clone, and the user's own `git fetch origin` afterwards.

## Decision

### 1. The origin URL names a reserved host the pool proxy answers itself

A source's origin is
`https://git.discobox.internal/api/project/{p}/pool/{q}/sandboxes/{id}/git-origins/{slug}.git`.
`git.discobox.internal` sits beside `api.discobox.internal`, the host the proxy
already answers for the discobox API (ADR 0140 §2): both are the pool's, under a
name reserved for private use, and neither resolves anywhere else.

The sandbox's git reaches it the way it reaches anything: through the egress
proxy its environment names, over the sandbox-local bridge that presents its
client certificate. The proxy intercepts the host whatever its allowlist says,
like the gate host, and never sends it to the internet.

### 2. The proxy forwards it to a loopback origin listener, as the certificate's sandbox

The proxy forwards a request for the origins host — its path and query, and
nothing else of where the sandbox sent it — to the pool agent's origin listener
on loopback, and names the sandbox its client certificate authenticated in a
header of its own, replacing whatever the sandbox sent under that name.

The listener serves the git-origins route alone, to the sandbox token alone,
and only when the token's sandbox, the path's sandbox and the proxy's are one.
A token copied out of one sandbox fetches nothing from another; the control
plane's tokens reach the same route on the agent's own listener, not here.

### 3. The token travels in the document

Each source whose origin the pool serves carries its token in the
runtime-config document (`originToken`), which the sandbox agent hands to git
through its credential helper and never writes into the checkout. The pool
issues it for a day and renews it when it has less than half of that left;
deciding the document renews it, and the status poll decides a running
sandbox's document every fifteen seconds.

## Alternatives rejected

- **The pool agent's own API port over the sandbox network.** The smallest
  change on a Docker pool, where the agent's TCP listener is already reachable.
  It does not exist on a VM pool, whose agent listens on VSOCK; it is
  plaintext and carries no client certificate, against ADR 0126 §7; and it
  puts the whole pool-agent API in a sandbox's reach for one route.
- **A pool listener of its own with a sandbox-local mTLS bridge, like the
  BuildKit mediator.** Compliant, and another listener, bridge unit, port and
  bootstrap endpoint for traffic the egress bridge already carries with the
  same certificate. The proxy already answers one reserved host for itself.
- **The control plane's git-origins route through the gate host.** A sandbox
  that creates discoboxes already pushes there. Every clone and fetch would
  cross the control plane and back to the pool that holds the repository, and
  the control plane does not take the pool's sandbox token.

## Consequences

- The proxy package gains a forwarded host (`proxy.OriginsConfig`) beside the
  gate host, and the pool agent a loopback listener beside its API.
- Every fetch of an origin is audited by the pool proxy like any other request.
- A sandbox reaches its origins on every backend whose sandboxes reach the
  proxy, which ADR 0126 §6 already requires of all of them.
