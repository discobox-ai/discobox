# Task — Pool-shared BuildKit

Implements [ADR 0044](../adr/0044-builds-run-on-a-pool-shared-buildkit.md).
Land the ADR as `Accepted` first; it is the spec.

Sequencing lives here, not in the ADR or any `DESIGN.md`. Delete or archive this
file when the work lands; the design docs carry the end state.

## Shape

```
sandbox                              pool container
-------                              --------------
docker build (shim)
  └─ buildx remote driver
     mTLS: /etc/discobox/proxy/  ──▶ mediator (pool-agent)
            client.{crt,key}          ├─ peer cert → sandbox id
                                      ├─ rewrites Control/Solve + LLBBridge/Solve
                                      └─▶ buildkitd (unix socket)
                                            PATH=/opt/discobox/bin
                                              └─ runc wrapper: CA mounts + hooks
     --push ──────────────────────▶ registry:2   ← build output store
  └─ docker pull + tag ◀────────────────┘
                                      RUN-step egress
                                        └─▶ per-build forwarder in that
                                            build's netns (sandbox cert)
                                              └─▶ pool proxy :17080
                                                    └─ blob cache (pull-through)
```

Two transports, deliberately separate: the **registry** stores build output
(decision 11); the **proxy cache** caches upstream pulls (decision 12).

## Status

Phase 1 has landed and is verified running in a live pool; `runcca` has moved to
the root module so the pool and the sandbox share one wrapper. Phase 6's two
contained cache fixes have landed. Phases 2–5 and 7 are untouched.

A pool build has been verified end to end: `buildctl build` against the pool's
buildkitd resolves `alpine:3.20` through the proxy, runs the `RUN` step through
the pool's runc wrapper, and prints its output.

**Resolved on the way there — stale network namespaces poison the state root
across a restart.** A build would fail at `load metadata for
docker.io/library/alpine` with `lookup auth.docker.io on 127.0.0.11:53: server
misbehaving`, even though `HTTPS_PROXY` was present in buildkitd's own
`/proc/<pid>/environ` and `curl` with that identical environment reached the
same URL fine. Every other suspect was eliminated by bisection: environment
(byte-identical between a working and failing instance), the config file, the
GC and parallelism flags, the state volume's filesystem, and startup ordering.
The differentiator was the *contents* of `--root`.

BuildKit keeps its network namespaces under `<root>/net/cni`. A namespace is a
bind mount of nsfs, so once the pool container restarts the mount is gone and
only an empty file remains on the ext4 state volume. CNI cannot release those
("unknown FS magic … ef53") and buildkitd comes up degraded, silently ignoring
its proxy. **This reproduced on a plain `docker restart` of the pool**, so it
was not a debugging artifact — builds would have worked exactly once per pool
and then failed permanently.

The fix is an `ExecStartPre` in `discobox-buildkitd.service` that clears
`${DISCOBOX_BUILDKIT_ROOT}/net` while leaving the build cache beside it intact.
Verified: clearing only that directory restored builds on the already-broken
instance.

## Phase 1 — Pool components (landed)

- [ ] `buildkitd` as a systemd unit in the pool image, mirroring
      `discobox-proxy.service`. State root on the pool cache volume
      (`layout.PoolCache`). One daemon per pool — `buildkitd.lock` is exclusive
      and the solver cache lives under `--root`, so a second daemon is not an
      option.
- [ ] `buildkitd.toml` with **explicit** `max-parallelism` (CPUs, floor 2) and
      GC thresholds sized to the pool cache volume. Defaults are derived from
      the host filesystem — an observed default `MaxUsed` of 93.13GiB was a
      fraction of the host disk, so every pool on a shared host would claim it.
- [ ] CNI plugins in the pool image; `--oci-worker-net=bridge` (reports as
      `worker.network:cni`). Keep `--oci-cni-pool-size` at 0 until netns reuse
      is exercised under concurrency.
- [ ] `registry:2` as a second unit, plain (not proxy mode) — it is the build
      output store. Needs `HTTP_PROXY` and pool CA trust like every other pool
      component; without egress it fails as an opaque 404.
- [ ] Registry GC policy and disk budget. The image exists transiently in
      buildkit's store, the registry, and the sandbox's docker.
- [ ] `runcca` deployed pool-side. **Execs `buildkit-runc`, not `runc`** — a
      wrapper that cannot find its real runc surfaces as exit 127 attributed to
      the user's build. Keep one package; make the binary path and hook
      behaviour parameters, not a fork.

## Phase 2 — Mediator

- [ ] Transparent gRPC proxy: `ForceServerCodecV2` + `UnknownServiceHandler`
      forwarding every method as opaque bytes. Verified to preserve
      `Control/Session` — filesync, `--secret`, `--ssh`, multi-platform and
      `--cache-to` all work through it.
- [ ] mTLS termination; peer cert CN is the sandbox id (client ID = sandbox ID
      is already the proxy's tenant boundary; no new key material).
- [ ] Decode and rewrite **both** solve methods. Build-args arrive on
      `/moby.buildkit.v1.frontend.LLBBridge/Solve` (`FrontendOpt`), not
      `Control/Solve` — buildx drives the dockerfile frontend through the
      gateway. Injecting only into `Control/Solve` silently does nothing.
- [ ] Override client-supplied proxy env rather than merging: buildx
      auto-forwards the client's `HTTP_PROXY`, which points at the sandbox's own
      loopback and is unreachable from a pool-side build container.
- [ ] Strip `network.host` and `security.insecure` from `Entitlements`. A
      host-network step runs in buildkitd's namespace, where the per-build
      loopback assumption collapses.
- [ ] Overwrite `SourcePolicy` / `SourcePolicies` on both methods, never merge.
- [ ] Log sandbox → `SolveRequest.Ref`. `Ref` is the build history ID (verified),
      and it is the only way to attribute sources to a sandbox: the gateway
      solve's `Definition` is nil for dockerfile builds, so the mediator cannot
      see identifiers itself.
- [ ] Bump `github.com/moby/buildkit` — v0.20.2 is pinned by nothing (the
      `// indirect` marker is stale; `server/providers/dockerworker/image_build.go`
      imports `buildkit/client` directly). v0.20.2 types do round-trip against a
      v0.32.2 daemon via unknown-field preservation, so this is not blocking.

## Phase 3 — Policy

- [ ] Watched JSON config with its own schema, mirroring
      `proxy.WatchConfigFile` (poll mtime → load → apply; invalid config keeps
      the previous policy). Pool-scoped, sibling to `layout.ProxySecretsFile`.
- [ ] `disablePolicy` as a **negative** flag so the zero value is safe: absent
      means policy on, empty rules mean deny-all. Code default
      `DisablePolicy: false`; pool-agent writes an explicit
      `"disablePolicy": true` for now. Note `LoadConfigFile` unmarshals *onto*
      defaults — a code default of `true` would silently disable enforcement for
      the first person who writes rules without also writing `disablePolicy: false`.
- [ ] Write the config at startup when absent, so "no file" never means
      accidental deny-all.
- [ ] Render the model to `pb.Policy` in one place, emitting the catch-all
      `DENY *` **first** (last match wins, not first). Test asserting
      `rules[0]` is the catch-all — the difference between an allowlist and a
      no-op is invisible by inspection.
- [ ] Always allow `local://*`. The catch-all matches `local://dockerfile` and
      the build context, so a policy omitting it denies every build.
- [ ] Rules carry sandbox IDs, like `AllowlistRule.ClientIDs`.

## Phase 4 — Per-build egress

- [ ] Wrapper installs `createRuntime` + `poststop` hooks into the OCI spec.
      BuildKit sets no `hooks` key at all, so there is nothing to merge with.
- [ ] `createRuntime` must not return until the listener is bound, or a step can
      start before its proxy exists. Measured bind latency ~11ms.
- [ ] Listener process pins its OS thread, `setns` on `/proc/<pid>/ns/net`, then
      `listen`. Without `runtime.LockOSThread` Go may create the socket on a
      thread still in the wrong namespace.
- [ ] Hooks are thin RPCs to pool-agent, which holds the certs and owns the
      sockets — hooks must exit, so they cannot be the forwarder or daemonize one.
- [ ] `poststop` teardown; verify no orphaned listener or pidfile.
- [ ] Identity rides in the injected proxy URL's userinfo. It is **not** a
      credential — the listener exists only in that netns — but strip it before
      the container sees it.

## Phase 5 — Sandbox

- [ ] Rewrite `sandbox-agent/dockercache`: delete `CacheDir`, the `index.json`
      probe and the legacy-to-buildx promotion; keep `buildCommand`/`nextOperand`.
      New job: `docker build -t X` → `buildx build --builder <pool> --push -t
      <pool-registry>/X`, then `docker pull` + `docker tag`.
- [ ] A shim is required: bare `docker build` **ignores** buildx's
      current-builder selection and pins the `default` instance. `--builder` and
      `BUILDX_BUILDER` both override it; `buildx use` / `use --global` do not.
- [ ] Provision the buildx remote builder at boot (`docker buildx create
      --driver remote` with the mTLS driver-opts against the already-staged
      `/etc/discobox/proxy/` material). Idempotent, and refreshed if the
      endpoint or cert paths change — `~/.docker` is on the persistent data
      volume, so a stale instance outlives what it points at.
- [ ] Leave `docker buildx` stock. `docker buildx use default` stays the escape
      hatch to the in-sandbox builder.
- [ ] Keep `runcca` in the sandbox for `docker run` — a nested run's bind mounts
      name sandbox paths that do not exist pool-side.

## Phase 6 — Proxy cache

`proxy/internal/cache` is complete and wired into `http.go`; it ships
`Enabled: false` with no `Patterns`. Four changes:

- [ ] Reject `206 Partial Content`. `ShouldCacheResponse` accepts `200..299`, so
      a range response is stored as a whole entity. `VerifyDigestHex` catches it
      for digest-bearing URLs, but returns nil when the path has no digest — so
      a truncated body is cached silently there.
- [ ] Add request coalescing (`singleflight` on the cache key). Ten sandboxes
      pulling the same uncached base layer currently make ten upstream fetches.
      This is the pool's primary workload; it is nginx's `proxy_cache_lock`.
- [ ] Key on the digest when the path carries one, instead of `Host + Path`, to
      dedupe the same layer across repositories. Safe only because pull
      credentials are pool-wide.
- [ ] Enable it with digest-scoped `Patterns` / `ContentAware` only. There is no
      TTL — correct for immutable blobs, wrong for anything else, so tag
      manifests must never be admitted.

Do **not** add credential injection. Every dominant bug class in
`rpardini/docker-registry-proxy` comes from it.

## Phase 7 — Docs

- [ ] `pool-agent/DESIGN.md`: buildkitd, mediator, registry, per-build egress.
- [ ] `sandbox-agent/DESIGN.md`: the shim's new job; `dockercache` package entry.
- [ ] `proxy/DESIGN.md` + `proxy/PLAN.md`: cache enabled, what it covers.
- [ ] Root `DESIGN.md` if the pool's component list is enumerated there.

## Risk register

Verified end to end, safe to build on: session passthrough; gateway-vs-control
injection point; hooks firing on `runc run`; `setns` listener reachable from the
build step; deny-all source policy; `CONVERT` with `$1` capture rewriting a
`FROM` to another registry; registry push/pull beating `--load` 5×; `Ref` ↔
build-history join.

Still unproven — treat as implementation risk:

- Everything was measured on one host where "remote" is a loopback TCP hop, with
  a throwaway wrapper rather than `runcca` itself. Behaviour inside a real pool
  container with a real sandbox across the pool network is untested.
- Why bare `docker build` ignores the current-builder selection is unexplained.
  It contradicts the `docker buildx use` docs and may be version-specific
  (measured on Docker 29.7.2 / buildx v0.36.1). The shim's explicit `--builder`
  is robust either way, but re-confirm on another host.
- netns reuse under concurrency (`--oci-cni-pool-size` > 0) could cross identity
  between builds if a forwarder outlives its build.
- Registry auth for a private upstream pull through the mediator.
- `registry:2` GC under sustained build load.

When re-testing anything: `buildkitsandbox` is the hostname of *every* BuildKit
build container including dockerd's embedded one, so it cannot distinguish a
local build from a remote one. Use `--debug`'s `#0 building with …` line, solve
counts at the builder, or `buildx du` deltas.
