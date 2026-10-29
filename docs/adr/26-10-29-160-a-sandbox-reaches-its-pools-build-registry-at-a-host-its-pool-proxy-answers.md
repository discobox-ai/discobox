# 26-10-29-160 — A sandbox reaches its pool's build registry at a host its pool proxy answers

- **Status**: Proposed (would supersede
  [0044](0044-builds-run-on-a-pool-shared-buildkit.md) §11's "a sandbox can
  pull images built by its peers",
  [0047](0047-local-base-images-resolve-through-a-per-sandbox-registry-namespace.md)
  §2's secret namespace, and
  [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
  §5's "`BuildKit` does not" and its deferral of builds behind an edge)
- **Date**: 2026-10-29
- **Relates to**:
  [0044](0044-builds-run-on-a-pool-shared-buildkit.md) §11 (build output
  travels through a pool-local registry),
  [0047](0047-local-base-images-resolve-through-a-per-sandbox-registry-namespace.md)
  (a per-sandbox namespace keyed on a secret, "a stopgap for the absent
  authentication"),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §7 (pool services are reached over authenticated network paths),
  [26-10-08-561](26-10-08-561-a-sandbox-reaches-its-origins-at-a-host-its-pool-proxy-answers.md)
  (a host the pool proxy answers itself, as the certificate's sandbox),
  [26-10-23-719](26-10-23-719-a-sandbox-may-reach-its-pool-through-a-websocket-tunnel-with-its-mtls-inside.md)
  §5 and Deferred (no tunnel carries BuildKit, so a pool behind an edge has no
  builder).

## Context

Every pool service a sandbox reaches is reached over mTLS with its pool-issued
client certificate (0126 §7) except one: the pool's build registry. The
sandbox's dockerd pulls a build's result, and pushes its local base images
(0047), from `discobox-pool-proxy:5000` over plaintext HTTP. `daemon.json`
lists it in `insecure-registries`, and the sandbox's `NO_PROXY` names
`discobox-pool-proxy`, so the pull skips the egress proxy and goes straight
over the pool's network. The registry authenticates nothing: build output is
protected by `discobox-build/<random hex>` being unguessable, and a sandbox's
base images by a namespace minted from a secret (0047 §2).

0126 §7 already says what follows: the registry's unguessable names "are not
protection once it is reachable off a private network: it authenticates pulls
and deletions, or it does not leave the pool." 719 draws the consequence for a
pool reached only through a TLS-terminating edge: the tunnel carries the
proxy, the credentials endpoint and DNS, but not BuildKit, because a build's
result would have to come back through a registry no tunnel may carry. Such a
pool has no builder. A machine pool whose sandboxes reach it at a URL that is
not a private network address (#129) would be the same.

The proxy already answers hosts that are the pool's own, under a reserved
name: `api.discobox.internal` (0140), `git.discobox.internal` (561) and
`assets.discobox.internal` (26-10-16-423 §13). A request for one is
intercepted whatever the allowlist says, forwarded to a pool-local upstream
with the sandbox its certificate names, and never cached or sent to the
internet. The registry is the same kind of thing.

## Decision

### 1. The registry is a host the pool proxy answers

A sandbox names the pool's build registry as `registry.discobox.internal`,
beside the gate, origins and assets hosts, and reaches it over HTTPS the way it
reaches anything: through the egress proxy its environment names, over the
sandbox-local bridge that presents its client certificate. dockerd trusts the
proxy's certificate for that host as it does for any egress, from the CA
bundle the sandbox already trusts. `insecure-registries` loses its entry, and
nothing names `discobox-pool-proxy:5000` from inside a sandbox.

The proxy forwards a request for the registry host — its path and query, and
nothing else of where the sandbox sent it — to a registry front the pool agent
serves on a Unix socket, naming the sandbox its certificate authenticated in
`PoolClientHeader`, replacing whatever the sandbox sent under that name. The
front decides on that header alone, so it is on a socket, as the assets
listener is: a loopback port is one a sandbox can reach as ordinary egress
with a header of its own.

### 2. A sandbox reads and writes its own namespace, and nothing else

The front admits a request only for repositories under the namespace of the
sandbox the header names, and forwards it to the registry. Everything else is
a 403 before the registry sees it, the catalog included.

- **The namespace is the sandbox ID**, as 0047 said it would be once the
  registry authenticates: the secret token, its staging, its place in the
  runtime-config document and `/etc/discobox/proxy/registry-namespace` go.
- **A build's result is pushed into the namespace of the sandbox that asked
  for it.** The mediator already takes the sandbox from the client
  certificate and rewrites each solve; it refuses a solve whose image export
  names a repository outside that sandbox's namespace, so no sandbox can have
  the builder write into another's.
- **A build reads and writes the registry within that namespace alone.**
  Every reference a solve names at the registry — an image export, a registry
  cache import or export, a frontend's `cache-imports` or `cache-from` — is
  rewritten from `registry.discobox.internal/<sandbox>/` to where buildkitd
  reaches the registry, and refused outside the namespace or when it names the
  registry's address itself. A source policy the mediator adds to each build
  does the same for every image the build reads: it converts the namespace's
  names, and denies the registry's address for any other namespace, after the
  sandbox's own rules so none of them can allow it.
- **A sandbox no longer pulls its peers' results** (0044 §11's "a sandbox can
  pull images built by its peers"). The shared solver cache is untouched, so a
  peer's build still makes the same build cheap; only reading another
  sandbox's output by name goes.
- **Deleting a sandbox's namespace** stays the pool's, as it is today.

### 3. The registry leaves the pool's network

The registry listens on the pool's loopback alone, where buildkitd pushes to
it. It is no longer reached on `0.0.0.0:5000` by anything on the pool's
network: the front is the only way a sandbox reaches it.

The registry speaks mTLS there and admits one client certificate, the pool
builder's, from a CA of its own: buildkitd presents it, configured for the
registry's address alone, and the front presents it. buildkitd resolves any
name a build gives it, and any name that resolves to loopback reaches the
registry's port; without the certificate it is refused, so the registry's
address is the one name a build can reach it by, and §2's binding is an exact
match on it rather than a guess at every alias.

Loopback alone is not that, because the pool proxy sends a sandbox's ordinary
egress from the pool's own network namespace: a request for
`127.0.0.1:5000`, or for any name that resolves there, reaches the registry
around the front. So the pool proxy refuses its own loopback as a
destination. It checks the address it actually dials, after resolution, so no
name reaches what an address may not; an unspecified address (`0.0.0.0`, `::`)
is refused with it. The pool's own hosts are not destinations: the proxy
forwards those to their upstreams itself (§1). An upstream proxy the pool
chains through is not one either.

### 4. The registry host is not cached, and its bodies are not spooled

Like the other pool hosts, a request for the registry host takes no secret
swap, no gateway and no response cache: the blobs are already on the pool's
disk, so caching them would only store each one twice. Each request is
audited as one, but its body is not spooled to the audit: a layer is
gigabytes, and the audit's byte bound (26-10-08-698) would spend itself on
build output.

### 5. The mediator can be reached through a tunnel

`PoolEndpoints.BuildKit` may be a `wss` URL like the other three, and the
tunnel listener gains a path for the mediator. With the registry on the
proxy, everything a build needs travels over a hop that carries the sandbox's
mTLS, so a pool whose sandboxes reach it only through an edge has a builder.
This supersedes 719 §5's "`BuildKit` does not" and its deferral of builds
behind an edge.

## Alternatives rejected

- **Authenticate the registry itself** — registry:2's token or htpasswd
  auth, with a per-sandbox credential in the runtime-config document. A second
  credential to issue, deliver and renew beside a certificate that already
  names the sandbox; the registry still listens on the pool's network in
  plaintext, and a pool behind an edge still needs a hop of its own for it.
- **A registry listener of its own with an mTLS bridge, like the BuildKit
  mediator.** Compliant with 0126 §7, and another listener, bridge unit, port,
  bootstrap endpoint and tunnel path for HTTPS traffic the egress bridge
  already carries with the same certificate. 561 rejected the same shape for
  origins.
- **Present the client certificate from dockerd** (`/etc/docker/certs.d`).
  Puts the sandbox's key where dockerd reads it, outside the bridges that keep
  it from ordinary processes (0126 §7), and still needs a listener and a
  tunnel path of its own.
- **Keep the registry off the tunnel** (719 §5 as it stands). A pool reached
  only through an edge, or a machine pool reached at a URL that is not a
  private address, has no builder.
- **Put the registry on a Unix socket**, where no egress can name it.
  buildkitd pushes to a registry by host and port and has no socket
  transport, so the registry stays on a port, and the proxy refuses the
  loopback it is on (§3).
- **Token authentication at the registry**, scoped per sandbox by tokens the
  front issues and a build fetches through its session. It would hold the
  registry to the namespace on every path, a custom frontend's included (see
  Consequences), but buildkit shares a cached registry authorization between
  sessions that present the same credentials, so each sandbox would need
  credentials of its own as well. Deferred: revisit if custom frontends are
  to be held to the namespace, or a sandbox ID stops being something a peer
  could not use against it.
- **Keep the secret namespace beside the authentication.** Once the front
  decides by certificate, a guessed or leaked token reaches nothing, and the
  secret is only one more thing to stage.

## Consequences

- Build output crosses the TLS-intercepting proxy: a pull is decrypted and
  re-encrypted once more than today, and costs the proxy's CPU. A cached
  rebuild still moves only what the daemon lacks (0044 §11).
- The sandbox's Docker feature changes: `daemon.json` drops the insecure
  registry, and the shim names `registry.discobox.internal` and the sandbox
  ID's namespace. A sandbox gets the new feature when its pins move; one whose
  docker feature predates this cannot pull a build's result until then, since
  the old listener is gone (§3).
- Images pushed under the old token namespaces are not reachable through the
  front. A base image is published again on its next build (0047 §3), and
  build output is transient, so nothing is migrated; the old repositories are
  removed with the rest of their sandbox's namespace or by the registry's
  garbage collection.
- A sandbox reaches nothing on its pool's loopback through the proxy. None
  had reason to: its own loopback is in its `NO_PROXY`, and the pool's
  services it does reach are the pool hosts, the gate and its mTLS
  endpoints.
- The mediator's binding covers what a solve it forwards names, and the
  source policy every image the build reads. A custom frontend (`# syntax=`)
  runs inside buildkitd and makes solves of its own that never reach the
  mediator: one could name another namespace's registry cache in a cache
  import there, which buildkitd reads with the pool builder's certificate. A
  sandbox ID is not a secret, so a sandbox that knows a peer's could read the
  peer's build cache that way — and, where the peer exported its cache inline,
  the layers a matching cache key leads to — but write nothing.
- 0126 §7's registry clause is met: the registry is reached over a
  connection carrying the sandbox's certificate, and authorizes by it.
- 0047's secret namespace and 0044 §11's cross-sandbox pull are superseded;
  719 §5's BuildKit exception and its deferral of builds behind an edge are
  superseded by §5.
