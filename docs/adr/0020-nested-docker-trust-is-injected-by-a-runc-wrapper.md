# 0020 — Nested Docker trust is injected by a runc wrapper, not an NRI plugin

- **Status**: Proposed
- **Date**: 2026-08-01
- **Supersedes**: [0015](0015-nested-docker-builds-trust-the-mitm-proxy-via-nri.md)

## Context

ADR 0015 established that nested Docker builds and containers must trust the
pool's MITM proxy without any awareness in a sandbox user's own Dockerfile. That
goal is unchanged and this ADR keeps it. What it replaces is the mechanism.

The NRI plugin 0015 specified never ran. Verified against a live sandbox with
everything 0015 called for in place — NRI enabled in `/etc/containerd/config.toml`,
the plugin registered and logging `Started plugin 00-discobox-ca` against
containerd v2.2.6, all three CA bundles staged in `/run/discobox/proxy/ca-bundles`,
`containerd-snapshotter` active — a `docker run` produced **no** `CreateContainer`
call at all. With the plugin rebuilt to log at debug level, container creation
emitted nothing. Containers received neither the CA mounts nor the proxy env.

The cause is structural, not configuration. containerd invokes NRI hooks only
from its CRI path (`io.containerd.cri.v1.runtime`). dockerd does not use CRI; it
drives the plain containerd client API, which has no NRI integration. No setting
changes this.

0015 decision 1 assumed `containerd-snapshotter` would produce "one containerd
instance, one interception point ... covers both build and run". It unified
where *layers* are stored, not where containers are *executed*. `docker buildx
inspect` reports `worker.executor: containerd`, yet BuildKit still invokes runc
directly against its own bundles under `/var/lib/docker/buildkit/executor`.

Two further defects were found while confirming this:

- Even had the plugin run, it injected `Env` verbatim, so a container would have
  received `HTTP_PROXY=http://127.0.0.1:17008` — the *container's* own loopback,
  where nothing listens. 0015 decision 7 required substituting the bridge
  gateway; no code did, and `NestedForwarderListen` was only ever used to bind
  the forwarder.
- Nothing gave **dockerd itself** proxy access. Image pulls happen in the daemon,
  before any container exists, so no container-level injection can reach them,
  and `docker.service` is socket-activated rather than spawned by sandbox-agent,
  so it inherits none of the sandbox's proxy env. Every image pull in a sandbox
  failed DNS resolution.

Alternatives considered for the replacement interception point:

1. **A docker `runtimes` entry plus `default-runtime` in `daemon.json`.**
   Verified working for `docker run` and verified *not* working for
   `docker build`: BuildKit's executor ignores `default-runtime`. Rejected as
   incomplete — it covers one of the two paths 0015 exists to cover.
2. **Replacing `/usr/bin/runc` with the wrapper.** Equivalent interception, but
   it fights the `runc` package on every upgrade. Rejected in favour of PATH
   ordering, which achieves the same thing without owning a dpkg-managed file.
3. **A BuildKit fork threading session identity into the executor.** Only
   relevant to a pool-shared builder (see Deferred); unnecessary here, since
   each sandbox has its own dockerd and the CA is pool-scoped anyway.

## Decision

### 1. A runc wrapper is the interception point, ahead of the real runc on PATH

`sandbox-agent/cmd/discobox-runc` installs to `/opt/discobox/bin/runc`. Drop-ins
prepend that directory to the PATH of **both** `containerd.service` and
`docker.service`. Both are required and neither covers the other:

- `docker run` → containerd shim → `runc create --bundle <dir>`
- `docker build` → BuildKit executor → `runc run --bundle <dir>`

runc is the one point every path converges on, which is precisely the property
0015 wrongly attributed to a single containerd instance.

The wrapper keys off the presence of `--bundle` rather than modelling runc's
global flag grammar: of runc's verbs only `create`, `run` and `restore` accept
it, and those are exactly the ones about to materialize a container from a spec.

### 2. The OCI spec is edited as a generic map, never as typed structs

The spec is decoded to `map[string]any`, mutated, and re-encoded, so every field
the wrapper does not model round-trips untouched. The OCI spec is large and
still growing; silently dropping a field runc depends on would be far worse than
not injecting at all. The rewrite is staged to a sibling temp file and renamed,
so a concurrent read never sees a partial spec.

### 3. Each trust store is seeded as a directory, not bound as a file

The obvious approach — bind the sandbox's bundle straight onto
`/etc/ssl/certs/ca-certificates.crt` — makes that path a mount point, and
Debian's `update-ca-certificates` replaces the bundle with `rename()`, which
fails `EBUSY` over one. Any Dockerfile running `apt-get install ca-certificates`
then dies with a dpkg error. This is not hypothetical: it broke this
repository's own `pool-agent` image, and it is inherited from 0015 decision 5,
which specified exactly that bind and was never exercised because the plugin
never ran.

Instead each trust store is seeded as a *directory*: its contents are copied to
a per-container staging directory under `/run` (already tmpfs, so the copy is
cheap and never outlives the sandbox), the MITM CA is appended to the aggregate
bundle there, and the staging directory is bind-mounted read-write over the
original. `rename()` inside it then works normally, and nothing lands in a
committed layer.

Two details are load-bearing:

- **Append, do not replace.** The image's own bundle is preserved and the CA
  added to it. Replacing it wholesale discards CAs the image shipped.
- **The rootfs is wherever the spec says.** `root.path` may be absolute or
  bundle-relative, and containerd's snapshotter points it at an absolute path
  under `/var/lib/docker` while leaving `bundleDir/rootfs` empty. Assuming the
  bundle-relative path silently finds nothing, which makes every seeded store
  fall back to the staged bundle and quietly drop the image's own CAs.

The raw CA is additionally dropped at each family's trust-anchor directory, so a
container that regenerates its bundle re-absorbs it rather than dropping it.
Those filenames are derived from the certificate's own fingerprint: a filesystem
can legitimately hold more than one Discobox MITM CA (one per pool, and a
sandbox nested inside another sees both its own and its host's). A fixed name
makes them collide, and because `discobox-trust-ca.service` installs its copy
with a write that cannot replace a bind mount, the collision fails `EBUSY` and
leaves the inner sandbox trusting the wrong CA.

### 4. Injection stays blind, and never overrides

Unchanged from 0015 decision 5: the wrapper cannot inspect the target image, so
it mounts every known trust-store path unconditionally (a bind onto a path the
image never reads costs nothing), and it leaves alone any mount destination or
env name the container already sets. Bind mounts are not part of a container's
upper/diff layer, so the MITM CA is never captured into a committed image —
verified by scanning a built image's OCI blobs: only the base layer's stock
bundle is present, never the staged one.

### 5. The loopback→bridge rewrite is driven by address, not by variable name

The wrapper reads the sandbox-local forwarder address from
`/etc/discobox/proxy/bridge.json` and the nested-Docker one from
`bridge-docker.json` — both already written by pool-agent — and rewrites any
occurrence of the former in a proxy-trust value to the latter. Matching on the
address rather than on a list of URL-valued variable names keeps 0015 decision
2's property intact: adding a proxy-trust variable remains a one-line change to
`EnsureSandboxMaterial`, with no second list anywhere.

### 6. dockerd gets proxy access from a rendered EnvironmentFile

`EnsureSandboxMaterial` writes `proxy.env`, a systemd `EnvironmentFile`
rendering of the same env map that populates `sandbox.json`, into the sandbox's
proxy material. `docker.service.d/proxy.conf` reads it with a leading `-`, so a
sandbox with no MITM proxy has no such file and dockerd starts unchanged. This
is a second *rendering* of one map, not a second list.

### 7. The nested-Docker bridge subnet is dockerd's to choose, not ours to pin

0015 decision 7 pinned `daemon.json`'s `bip` to `172.30.0.1/16` so the
bridge-facing forwarder could `FreeBind` a known address before `docker0`
existed. That claims a fixed slice of RFC1918 space in every sandbox at every
nesting level: it collides with whatever the surrounding environment routes
(`172.16.0.0/12` is heavily used in corporate networks), burns one of Docker's
fifteen default /16s per level, and — demonstrated — gives a nested sandbox a
`docker0` overlapping the very network its own `eth0` sits on, because the host
sandbox pinned the same value.

dockerd already picks a non-overlapping subnet on its own, so `bip` is removed
and the pin goes with it. The cost is that the forwarder can no longer pre-bind:

- The socket unit is replaced by a `docker.service.d` drop-in with
  `Upholds=`, the same pattern 0015 decision 6 used to hang the NRI plugin off
  containerd. The forwarder runs only while dockerd runs, preserving the
  on-demand property without needing a known address.
- It waits for the bridge to appear (dockerd creates it asynchronously), binds
  whatever dockerd chose, and publishes that address under `/run`.
- The runc wrapper reads the published address rather than a constant. When
  none is published it injects the CA but **omits** the proxy variables: an
  unreachable proxy address hangs every client in the container, which is worse
  than leaving them unset and failing honestly.

pool-agent stops owning the address entirely. It emits a
`sandboxconfig.LocalSubnetsToken` placeholder in `NO_PROXY`, which sandbox-agent
resolves to the real directly-connected networks when it materializes a
container's environment — neither side needs the other's knowledge, and the set
is enumerated late because the bridge and any user-created networks appear after
boot.

### 8. NRI is removed entirely

The `nrica` package, `cmd/discobox-nri-ca`, `discobox-nri-ca.service`, the
`containerd.service.d` drop-in that upheld it, and
`/etc/containerd/config.toml` (which existed only to enable NRI) are all
deleted, along with the `containerd/nri` dependency. Leaving a plugin that
provably never fires would mislead the next reader into building on it.

## Consequences

- Sandbox users' Dockerfiles and `docker run` invocations still need no MITM
  awareness — but now actually receive trust. Verified end to end: inside a
  `RUN` step, all 12 proxy-trust variables present, the staged bundle mounted,
  and `curl https://example.com` returning 200 through the MITM proxy.
- Image pulls work at all, which they previously did not.
- The wrapper is on the hot path of every container start. It is a single
  process exec that reads three small files, and any failure is reported and
  stepped over — a container that starts without the CA fails only the TLS
  calls it makes, whereas a container that fails to start breaks the sandbox.
- `containerd.service.d/runc-ca.conf` and `docker.service.d/runc-ca.conf` must
  be kept in step. Covering only one silently loses either every `docker run`
  or every `docker build`.
- The sandbox image no longer ships a containerd config override, so containerd
  runs on its packaged defaults.
- ADR 0015's decisions 2 and 3 (the env map as the single naming point, and
  `ProxyEnvs`) survive unchanged. Decision 4's boot-time bundle staging survives
  as a *fallback* only, for images that ship no bundle of their own. Decisions
  1, 5, 6 and 7 are replaced.
- The sandbox no longer pins a bridge subnet, so nested Docker stops colliding
  with the surrounding environment's RFC1918 space, and a sandbox running inside
  another sandbox gets a bridge that does not overlap its own uplink.
- `containerd.service.d/runc-ca.conf` and `docker.service.d/runc-ca.conf` must
  be kept in step. Covering only one silently loses either every `docker run`
  or every `docker build`.
- Verified end to end at two nesting levels: a sandbox created inside a sandbox
  reaches real HTTPS through four chained MITM proxies, each with its own CA.

## Deferred

- **A pool-shared BuildKit builder.** Attractive for shared build cache, but a
  shared worker presents one client identity to the proxy, while per-sandbox
  identity is load-bearing for secret grants and audit attribution
  (`proxy/DESIGN.md`: "client identity is the tenant boundary"). A RUN step's
  OCI spec carries no session or tenant identity — verified: `annotations` is
  null, `hostname` is the constant `buildkitsandbox` — so a spec-level
  interceptor cannot attribute a build to its sandbox. Revisit only with a
  mechanism that binds each build's egress to its owning sandbox's client
  certificate.
- **A pull-through caching registry.** The proxy already has an unused response
  cache (`proxy/internal/cache`, wired to a pool-scoped directory but never
  enabled). Enabling it would cache registry blobs on the existing audited
  path, whereas a mirror would route pulls around the MITM proxy and lose
  per-sandbox attribution. Revisit with the cache's auth-keying settled: a
  blob one sandbox authenticated for must not be served to another.
