# 0044 — Builds run on a pool-shared BuildKit, bound to a sandbox by a mediator

- **Status**: Accepted
- **Date**: 2026-08-11
- **Resolves**: the deferred "pool-shared BuildKit builder" item in
  [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md)

## Context

`sandbox-agent/dockercache` installs a `docker` CLI shim at `/usr/local/bin/docker`
that rewrites `docker build` to add `--cache-from`/`--cache-to` against a
directory on the pool-shared cache volume. It exists for one reason, stated in
its own package doc: those flags exist only on the command line, so the command
line is the only place to inject them.

The shim buys layer reuse between a pool's sandboxes, but each sandbox still
builds in its own nested BuildKit. Every sandbox re-imports and re-exports the
cache as an OCI layout, duplicating work, and the package documents a residual
failure mode where two concurrent exports collide on `index.json`'s `TryLock`
and fail the solve.

That export is not incidental. Measured on a 641MB image, `--cache-to
type=local,mode=max` spent 2.4s sending 304MB — comparable to what `--load`
costs for the image itself — and `mode=max` is deliberately larger than the
image, since exporting intermediate stages is what lets a *different* Dockerfile
hit the cache. Every build in every sandbox pays it, re-exporting layers its
neighbours already exported.

A pool-shared builder removes both, and removes the need for cache flags
entirely: the cache stops being something to point at and becomes the builder's
own state.

0020 deferred exactly this, and set the bar:

> A RUN step's OCI spec carries no session or tenant identity — verified:
> `annotations` is null, `hostname` is the constant `buildkitsandbox` — so a
> spec-level interceptor cannot attribute a build to its sandbox. Revisit only
> with a mechanism that binds each build's egress to its owning sandbox's
> client certificate.

That finding still holds; it was re-confirmed against BuildKit v0.32.2. What
changed is the conclusion drawn from it. 0020 assumed the interception point had
to be the OCI spec, where identity provably does not exist. Identity can instead
be established at the BuildKit API layer, where the client's mTLS certificate is
available, and *carried into* the spec.

### Why identity cannot be reachability alone

Sandbox egress identity is established by private reachability, not by a
credential: a sandbox's own processes reach a loopback forwarder, and its nested
containers reach a forwarder on that sandbox's Docker bridge. Both addresses are
private to the sandbox, so arriving on one *is* the proof of identity. This is
deliberate — most software cannot present a client TLS certificate to a proxy,
so the forwarder holds the certificate and speaks plaintext to its clients.

A pool-shared builder breaks that property. `--oci-worker-net` is daemon-global
(`auto|bridge|cni|host`), with no per-solve network configuration, so every
build step from every sandbox lands in one reachability domain. Address alone
can no longer distinguish sandboxes.

### What was verified

Against BuildKit v0.32.2, with a throwaway gRPC mediator and a runc wrapper that
dumped OCI specs:

- Proxy build-args reach every `RUN` with no `ARG` declaration, **verbatim,
  including arbitrary URL userinfo**.
- They are **excluded from the cache key** — building with `http://alpha:1111`
  then `http://beta:2222` reported `CACHED`. Without this, per-sandbox proxy
  addresses would fragment the cache per sandbox and defeat the entire purpose.
- They are excluded from image config and `docker history`.
- The build step's network namespace is a **path in the OCI spec**
  (`/var/lib/buildkit/net/cni/<id>`), created before runc, and `lo` is up
  inside it (a loopback probe returned *connection refused*, not *network
  unreachable*).
- runc supports all six OCI hooks, and **BuildKit sets none** — the spec's
  top-level keys are `hostname, linux, mounts, ociVersion, process, root`.
- `buildkitd` holds an exclusive `buildkitd.lock` on its state root, and the
  solver cache (`cache.db`) lives there. Pointing several daemons at one
  containerd shares blobs but not cache records.
- A raw-codec gRPC passthrough preserves `Control/Session`: local build context
  (filesync), `--secret`, `--ssh`, multi-platform, and `--cache-to` all work
  through the mediator.
- BuildKit v0.20.2 client types round-trip against a v0.32.2 daemon, because
  protobuf-go preserves unknown fields.
- The full per-build forwarder mechanism works end to end. A wrapper installed
  `createRuntime`/`poststop` hooks into the spec; the hook entered the build's
  netns by `setns` on `/proc/<pid>/ns/net` and bound `127.0.0.1:17008` there;
  the `RUN` step reached it and read back that container's own id
  (`per-build-forwarder container=6cfqrz0wifsj9n3j8v3gjpok6`). Bind took 11ms,
  the whole lifecycle 149ms, and `poststop` left no listener or pidfile behind.

## Decision

### 1. Builds move to a pool-shared BuildKit; `docker run` does not

One `buildkitd` per pool, run as a systemd unit in the pool container beside the
proxy unit, with its state on the pool cache volume. One daemon is not a
preference: the solver cache is bbolt under an exclusively locked state root, so
two daemons cannot share a build cache at all.

Nested Docker stays in the sandbox. A build's inputs are content-addressed and
travel over the session; a `docker run`'s inputs include arbitrary host paths
that exist only in the sandbox's filesystem and cannot be shipped anywhere.
**Builds are transferable, runs are not** — that asymmetry, not convenience, is
why the split falls where it does.

### 2. A mediator in pool-agent is the identity boundary

Sandboxes reach `buildkitd` only through a gRPC mediator that terminates mTLS.
The peer certificate is the sandbox's existing proxy client certificate
(`/etc/discobox/proxy/client.{crt,key}`, client ID = sandbox ID, already the
proxy's tenant boundary), consumed by buildx's `remote` driver via its
`cacert`/`cert`/`key`/`servername` driver-opts. No new key material.

The mediator forwards every method as opaque bytes and decodes only the solve
methods. `buildkitd`'s own TLS authenticates pool membership, not which sandbox,
so the mediator — not the daemon — is where per-sandbox decisions are made.

### 3. Per-build egress is a forwarder in that build's own network namespace

The mediator injects a per-build proxy address as a build-arg. The pool's runc
wrapper reads it from the spec, resolves it to the owning sandbox, and — via
`createRuntime` and `poststop` hooks it installs into the spec — has pool-agent
bind a plaintext forwarder holding that sandbox's client certificate inside the
build's own network namespace.

This reconstructs private reachability one level down: the listener exists only
inside that one netns, so the identifier in the proxy URL **is not a credential**
and is inert if leaked. Identity is bound to the namespace, not to a secret.
This is what satisfies 0020's condition.

Hooks, not supervision. runc supports them, BuildKit sets none, so the field is
unclaimed, and `poststop` fires even when a container dies badly. The wrapper
therefore stays an exec-through shim that edits a spec — it does not fork, wait,
forward signals, or preserve exit codes.

`createRuntime` must not return until the listener is bound, or a build step can
start before its proxy exists. The hook waits on a readiness file; measured bind
latency is ~11ms.

The listener is created by a process that pins its OS thread, `setns`es into the
namespace, and only then calls `listen`. Go schedules goroutines across threads,
so a socket created without `runtime.LockOSThread` may be bound in the wrong
namespace.

The wrapper execs `buildkit-runc`, not `runc`. A wrapper that cannot find its
real runc fails as an opaque exit 127 attributed to the user's build.

### 4. Proxy env is injected at the gateway solve, not `Control/Solve`

buildx drives the dockerfile frontend through the gateway, so `Control/Solve`
carries `frontend=""` and the build-args arrive in
`/moby.buildkit.v1.frontend.LLBBridge/Solve`'s `FrontendOpt`. Injecting into
`SolveRequest.FrontendAttrs` alone has no effect — verified, and it fails
silently by leaving the variable empty rather than erroring.

Both methods are handled, because a `buildctl` client can take the direct path.

### 5. CA trust remains a spec edit, because it needs no identity

The MITM CA is pool-scoped (`PrepareBundle(projectID, poolID)`), so every
sandbox in a pool already trusts the same one. The runc wrapper injects it
unconditionally, exactly as in the sandbox. Only the *proxy address* needs
identity, which is why the two halves of 0020's injection split here.

### 6. Source policy is the per-sandbox control for non-exec traffic

Registry pulls, `ADD <url>`, and git contexts happen inside `buildkitd`, in no
container and no per-build netns, so their egress is attributed to the pool.
This is accepted.

Per-sandbox *control* is retained, because `SourcePolicy` is per-solve: the
mediator sets it from the mTLS peer identity. Verified — an injected deny-all
produced `source "local://dockerfile" denied by policy`. This is enforced before
the fetch, so a denied source opens no connection at all.

Source policy governs **identifiers, not destinations**; a permitted pull's
blobs may be redirected to hosts the policy never named. The proxy allowlist
remains the coarse network backstop for that traffic, and for `buildkitd`'s own
connections it is necessarily the union across the pool. It is not the
per-sandbox control and must not be described as one.

What is genuinely lost: per-sandbox audit rows for pull traffic, and per-sandbox
sentinel resolution of registry credentials — the latter a future cost, since
the proxy runs with a nil resolver today. Credentials themselves are unaffected;
they still arrive per-sandbox through the session's auth attachable.

### 7. Policy is file-driven, defaults to on, and blank means deny

The mediator watches its own pool-scoped JSON config, mirroring
`proxy.WatchConfigFile`. It has its own schema: the proxy speaks domains and
IPs, source policy speaks scheme-prefixed identifiers with wildcards, and
`CONVERT` has no proxy analogue. Unifying the two policy surfaces is a later
decision.

`disablePolicy` is a negative flag so the zero value is the safe one: absent
means policy is **on**, and an empty rule set denies everything. The code default
is therefore `DisablePolicy: false`, and pool-agent writes an explicit
`"disablePolicy": true` today — the permissive state is a string in a file, not
a default someone has to know about. This matters because `LoadConfigFile`
unmarshals *onto* the defaults, so a code default of `true` would silently
disable enforcement for the first person to write rules without also writing
`disablePolicy: false`.

pool-agent writes the config at startup when absent, so "no file" never means
deny-all-by-accident.

Two properties are load-bearing. The mediator **overwrites** the policy fields
on both solve methods rather than merging. This is defense in depth rather than
the sole protection: a deny at `Control/Solve` survives an `ALLOW *` policy
attached to the gateway solve — verified by simulating a hostile client, which
was still refused with `source "local://dockerfile" denied by policy`. A real
buildx client sends no gateway policies at all (`policies=0` observed).

And a rendered allowlist must always permit `local://*`: the catch-all `*`
matches `local://dockerfile` and the build context, so a policy that omits it
denies every build.

### 8. The `docker` CLI shim survives, with a much smaller job

The shim is kept, but `dockercache`'s cache machinery — `CacheDir`, the
`index.json` probe, the legacy-to-buildx promotion — is deleted. A shared
builder has no cache flags to inject. What remains is builder selection and
image transport: rewrite `docker build -t X` to
`buildx build --builder <pool> --push -t <pool-registry>/X`, then `docker pull`
and `docker tag` so the user still ends up with `X` locally (see decision 11 for
why `--push` rather than `--load`). Everything else passes through untouched,
and the existing argument-walking (`buildCommand`, `nextOperand`) is reused.

A shim is required because **`docker build` ignores buildx's current-builder
selection and pins the `default` instance**. `docker build` and
`docker buildx build` are the same command — `docker build --help` prints
`Usage: docker buildx build` — yet with one builder in a clean `DOCKER_CONFIG`,
buildx's own `--debug` output reports different choices:

```text
docker build         → #0 building with "default" instance using docker driver
docker buildx build  → #0 building with "only" instance using remote driver
```

Confirmed independently: after a bare `docker build`, `docker buildx du` had
grown on the `default` instance and was unchanged on the selected one. Builder
precedence is therefore:

| | `docker build` | `docker buildx build` |
| --- | --- | --- |
| `--builder` flag | wins | wins |
| `BUILDX_BUILDER` env | wins | wins |
| `buildx use` / `use --global` | **ignored** | honored |

`BUILDX_BUILDER` routes but leaves the image in the build cache unless every
invocation also passes `--load`, failing the out-of-the-box requirement.
`docker build --builder <name> --load` both routes and loads. Only a shim can
supply those flags unconditionally.

Whether the `default` pinning is a bug or an undocumented compatibility
carve-out is unresolved — it contradicts the `docker buildx use` documentation
("build commands invoked after this command will run on the specified builder").
The choice of explicit flags is deliberately robust to that: the flag has
highest precedence either way, so a future buildx that honors `current` for
`docker build` changes nothing here. Measured on Docker 29.7.2 with buildx
v0.36.1; worth re-confirming on other versions.

Note for anyone re-testing: `buildkitsandbox` is the hostname of *every*
BuildKit build container, including dockerd's embedded one, so it cannot
distinguish a local build from a remote one. Use `--debug`'s
`#0 building with …` line, solve counts at the builder, or `buildx du` deltas.

`docker buildx` is left entirely stock — the shim rewrites only `docker build`.
Explicit user choices still win, preserving `dockercache`'s existing principle.

### 9. `network.host` and `security.insecure` stay denied

A host-network build step runs in `buildkitd`'s namespace, where the per-build
loopback assumption collapses and every other build's forwarder is reachable.
BuildKit denies both entitlements by default; the mediator additionally strips
them from `SolveRequest.Entitlements`.

### 10. Parallelism and GC are set explicitly

BuildKit derives GC defaults from the filesystem it sees, not from any pool
allocation — an observed default of `MaxUsed 93.13GiB` was a fraction of the
host disk. Several pools on one host daemon would each believe they may use it.
`max-parallelism` and the GC thresholds are therefore pinned in the pool image's
`buildkitd.toml`, sized to the pool's cache volume.

### 11. Built images travel through a pool-local registry, not `--load`

`--load` serialises the whole image as a `docker save` tarball over the session.
The receiving end is `docker load`, which exposes no "do you already have this
blob?" API, so there is nothing to negotiate: **every build ships the entire
image, even when the build was fully cached.**

A registry is content-addressed on both ends — push does `HEAD /v2/…/blobs/…`
and uploads only what is missing, pull fetches only what the daemon lacks.
Measured on a 641MB image, cached rebuild:

| | wall |
| --- | --- |
| remote builder + `--load` | 6.3s (4.8s `sending tarball`) |
| local `default` builder | 3.0s |
| **push to registry + pull** | **1.23s** (0.92s push, 0.31s pull) |

Cold builds are a wash (~11s either way); cached rebuilds are 5× better, and
the registry path beats even building in-sandbox. A plain `registry:2` in the
pool is the store. It is a store, not a cache — see decision 12 for the
separate caching concern.

The registry serves the pool, so a sandbox can pull images built by its peers.
That is the pool sharing boundary again, and it is the same boundary the shared
build cache already has.

### 12. Upstream pull caching belongs to the proxy, not a registry mirror

Caching upstream registry pulls is a *different* job from storing build output,
and a registry is the wrong component for it: `registry-mirrors` in
`daemon.json` only mirrors Docker Hub, so a pool registry cannot transparently
intercept a `docker pull ghcr.io/…` from inside a sandbox. The proxy sees
everything, because it is the only way out.

`proxy/internal/cache` already implements this and is merely switched off. It is
registry-aware by construction: `ContentAware` matches `sha256:` paths with
Docker/OCI `Accept` headers, `isDockerResponse` checks `Docker-Content-Digest`
and OCI media types, and `StreamingPut` hashes the body while streaming so
`VerifyDigestHex` can abort a write whose bytes do not match the digest in the
URL. That integrity check is stronger than the reference implementation in this
space (`rpardini/docker-registry-proxy`, nginx-based) provides.

One existing decision is load-bearing and worth naming: `GenerateKey` is
`Host + Path`, **excluding the query string**. Registry blob GETs answer 307 to
a signed CDN URL whose signature lives in the query and changes per request
(verified against Docker Hub: `production.cloudfront.docker.com/…?Expires=…&Signature=…`).
Keying on the full URL would give a 0% hit rate, silently. Dropping the query
collapses those onto one stable key, which is what the reference implementation
needs an explicit redirect-and-rekey dance to achieve.

Credential injection is deliberately **not** adopted. Every dominant bug class
in the reference implementation comes from injecting upstream auth — a 401 issue
open since 2020 with 20 comments, per-registry breakage for ACR, GAR and
Artifactory, and a 2026 fix for basic auth clobbering clients' own bearer
tokens. Blobs are content-addressed, so caching them is correct regardless of
who authenticated; clients keep sending their own credentials. Pool-wide pull
secrets remain a separate, later decision.

## Consequences

- A pool's sandboxes share one build cache and one image store. Private base
  image layers pulled by one sandbox are reachable by its peers. This is not a
  regression: `dockercache` already shares a `mode=max` cache directory across
  the pool, and the pool is already the sharing boundary for cache.
- Builds queue. One shared daemon introduces head-of-line blocking that
  per-sandbox builders did not have, and cross-sandbox cache eviction under GC.
  Per-sandbox admission control is explicitly out of scope; the mediator is
  where it would go, since it already sees every solve with an identity
  attached.
- `runcca` is deployed twice — sandbox and pool — with different real-runc
  binary names. 0020's warning that the containerd and docker drop-ins must be
  kept in step now extends to a third site. One package, differences as
  parameters.
- The mediator cannot enumerate a build's sources directly. For a dockerfile
  build the gateway solve's `Definition` is nil, because the frontend generates
  LLB inside the daemon, so the mediator can set policy but cannot itself log
  which images a build pulled.

  It is recoverable by a join rather than lost: `SolveRequest.Ref` is the build
  history ID — verified, a solve carrying `ref="pmzgn5a1w2ai4jx0q4mphdt7f"`
  appeared under exactly that BUILD ID in the daemon's history. The mediator
  therefore records sandbox → `Ref`, and BuildKit's history and provenance
  records `Ref` → sources. Per-sandbox source attribution needs pool-agent to
  consume the build history API; it is not available from the mediation point
  alone.
- The `index.json` `TryLock` collision documented in `dockercache` disappears,
  along with per-sandbox cache import/export overhead.
- Image transport moves to the registry (decision 11), so a cached rebuild costs
  1.23s rather than `--load`'s 6.3s on a 641MB image. The image transiently
  exists in three places — buildkit's store, the pool registry, the sandbox's
  docker — so the pool needs disk budgeted for all three, and the registry needs
  a GC policy of its own.
- The pool gains two new stateful components (buildkitd, registry) on top of the
  proxy. Both need `HTTP_PROXY` and pool CA trust to reach upstreams, since pool
  egress crosses the same MITM proxy as everything else. A registry that cannot
  resolve its upstream fails as an opaque 404 rather than a connection error —
  observed while testing.
- `buildkitd` is a new privileged component in the pool container, needing CNI
  plugins for bridge networking and disk budgeted on the pool cache volume.
- `server/providers/dockerworker/image_build.go` imports `buildkit/client`
  directly despite the stale `// indirect` marker on the v0.20.2 requirement.
  The mediator wants current control and gateway protos, so the dependency is
  bumped.

## Deferred

- **Per-sandbox attribution for non-exec fetches.** Requires threading session
  identity into BuildKit's source-op transports — the fork 0020 rejected as
  "only relevant to a pool-shared builder", a qualifier that no longer holds.
  Possibly upstreamable rather than a permanent fork. Revisit if per-sandbox
  registry secret grants become a hard requirement; the alternative shape is
  per-sandbox daemons with mediator-injected `SolveRequest.Cache` against a
  pool-local registry, trading disk and native cache hits for attribution.
- **BuildKit's own `--proxy-network`.** v0.32 ships `util/network/proxyprovider`:
  a per-build netns pool, its own MITM CA injected into container trust stores,
  request capture for provenance, and source-policy evaluation of proxy
  requests. It overlaps decisions 3 and 5 substantially. It is not adopted here
  because nothing documented configures an upstream proxy per solve, so build
  egress could not be attributed or chained to the pool proxy. Revisit if
  per-solve upstream configuration appears.
- **A pull-through caching registry.** Unchanged from 0020.
