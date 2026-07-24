# 0015 — Nested Docker builds and containers trust the MITM proxy via an NRI plugin

- **Status**: Accepted
- **Date**: 2026-07-23

## Context

Every sandbox is provisioned with a per-sandbox client identity behind
Discobox's own pool-scoped MITM proxy (`proxy` package), so all sandbox egress
can be policy-checked and audited. `discobox-trust-ca.service` already installs
the pool's MITM CA into the sandbox VM's system trust store
(`proxy.SystemCABundle`) at boot, so processes launched directly by the
sandbox-agent trust the proxy transparently.

The sandbox image also ships `docker-ce`/`docker-buildx-plugin` so users can run
Docker builds and containers *inside* their sandbox. Each `RUN` instruction in a
Docker build, and each `docker run` container, executes in its own filesystem
and process namespace that does not see the sandbox VM's trust store update.
Any networked step in a user's own Dockerfile (`apt-get`, `curl`, `pip install`,
`npm install`, ...) that crosses the MITM proxy fails TLS verification unless
something re-establishes trust inside that specific container.

Three alternatives were considered for closing this gap:

1. **`RUN --mount=type=secret`** (BuildKit build secrets). This is the
   standard, fully-supported mechanism for giving a build step access to
   material that must never land in a committed layer. It was rejected as the
   primary mechanism because it is opt-in per `RUN` instruction: it requires
   editing every user Dockerfile that makes a networked call. Sandbox users'
   Dockerfiles are theirs, not Discobox's, and requiring MITM-awareness in
   arbitrary user build files is not acceptable — the MITM proxy is an
   infrastructure concern the sandbox imposes, not something a sandbox user's
   own build should need to know about.
2. **A custom BuildKit Dockerfile frontend** (`# syntax=...`) that auto-injects
   a secret-mount-and-trust step ahead of the user's instructions. This gets
   closer to zero Dockerfile changes but still requires one line per
   Dockerfile, and only covers `docker build`, not plain `docker run`
   containers a user starts directly.
3. **Bake the MITM CA into the sandbox's base images.** Rejected outright:
   sandbox images are sometimes the thing a user exports or otherwise cares
   about as an artifact, and the MITM CA must never be shippable inside any
   image content, only usable transiently while the sandbox runs behind the
   proxy.

## Decision

### 1. Unify `docker build` and `docker run` onto one containerd instance

Enable `"features": {"containerd-snapshotter": true}` in the sandbox image's
`/etc/docker/daemon.json`. `docker run` containers already execute as
containerd tasks regardless of this flag; enabling the containerd snapshotter
additionally routes dockerd's embedded classic-build BuildKit `RUN` steps
through the same containerd instance instead of BuildKit's separate OCI
worker + overlayfs path. One containerd instance, one interception point,
covers both build and run.

### 2. The `proxy` package's `ClientMaterial.EnvironmentVars` map is the one place that names proxy-trust env vars

`proxy/certs.go`'s `EnsureClientCertificate` already builds
`ClientMaterial.EnvironmentVars` — today `HTTP_PROXY`, `HTTPS_PROXY`,
`ALL_PROXY`, `NO_PROXY`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` — as the single,
pool-agent-owned statement of what a sandbox client needs set to trust the
proxy. This map already flows into `sandboxconfig.Document.Runtime.Env` today
(`pool-agent/sandboxruntime/runtime.go`'s `buildSandboxDocument`), just
indistinguishably from every other env var a sandbox happens to carry.

This ADR extends that one map, not the NRI plugin, when a new tool or runtime
needs a different override variable (`NODE_EXTRA_CA_CERTS`, `PIP_CERT`,
`CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, ...). The NRI plugin never hardcodes env
var names; it only republishes whichever names the `proxy` package declared.

### 3. `sandboxconfig.RuntimeLayer` gains `ProxyEnvs []string` naming which `Env` keys are proxy-trust vars

`RuntimeLayer.Env` keeps carrying the actual values (unchanged: sandbox boot
already applies `Env` to the sandbox's real process environment). A new
runtime-owned field, `ProxyEnvs []string` (`json:"proxyEnvs,omitempty"`),
carries just the *names* — the keys of `ClientMaterial.EnvironmentVars` that
`buildSandboxDocument` merged into `Env` for this sandbox. This is the
consolidation point: adding a new proxy-trust env var means adding one entry
to the `proxy` package's map, and it automatically shows up in `ProxyEnvs` for
every consumer — no second list to keep in sync.

At boot, sandbox-agent computes the name→value subset of `Env` that
`ProxyEnvs` names and writes it as one file,
`/etc/discobox/proxy/proxy-env.json`, for the NRI plugin to read. This carries
values, not just names: `discobox-nri-ca.service` is a separate systemd unit
and does not inherit `discobox-sandbox-agent`'s process environment, so "read
the value back from the plugin's own environment" has nothing to read from —
once the plugin needs the values delivered explicitly anyway, there is no
reason to split names and values across two files.

### 4. `discobox-trust-ca.service` also prepares non-Debian bundle formats, once, at boot — not per-container

Today this unit only updates the Debian PEM bundle
(`proxy.SystemCABundle`). Extend it (or add a sibling unit in the same
boot sequence) to also produce, once per sandbox boot, whatever additional
trust-material formats a container's rootfs might expect, staged under a
fixed `/etc/discobox/proxy/ca-bundles/` directory:

- The same PEM bytes, staged under the path conventions of other PEM-based
  distros (Alpine's `/etc/ssl/cert.pem`; RHEL-family's
  `/etc/pki/tls/certs/ca-bundle.crt`) — no new format, just additional
  destination copies of one source file.
- A Java `cacerts` keystore, generated via `keytool -importcert` when a JDK
  toolchain is present on the sandbox, for containers that run a JVM.
- Further formats (an NSS `cert9.db`, for example) are explicitly not
  designed here — this ADR does not know the full set a real workload will
  need. `discobox-trust-ca.service` becomes the single place new formats get
  added as gaps are found, so the NRI plugin never generates a format itself
  at container-creation time; it only picks from what boot already prepared.

### 5. An NRI plugin injects mounts and env into every container spec at creation time, best-effort per container

Enable NRI in the sandbox's `/etc/containerd/config.toml`
(`[plugins."io.containerd.nri.v1.nri"] disable = false`) and ship a new binary
in the `sandbox-agent` nested Go module, following the existing
`pool-agent/cmd/discobox-vsock-guest` + `pool-agent/vsock` split: a thin
`sandbox-agent/cmd/discobox-nri-ca/main.go` entrypoint over a sibling
`sandbox-agent/nrica` package that owns the `containerd/nri/pkg/stub`
`CreateContainer` implementation. It is cross-compiled in the same builder
stage as `discobox-sandbox-agent` and runs as a long-lived process
(`discobox-nri-ca.service`) that dials and stays registered against
containerd's NRI socket for the sandbox's lifetime — unlike the one-shot
`discobox-trust-ca.service`, this must reconnect if containerd restarts. The
plugin implements `CreateContainer`:

- **Bind-mount, never write.** For each pre-staged bundle from decision 4, the
  plugin bind-mounts it read-only into the new container. A mount is not part
  of a container's upper/diff directory, so nothing added this way is ever
  captured in a committed image layer. The plugin never runs
  `update-ca-certificates` (or any trust-store rebuild command) inside the
  target container — that would mutate a real rootfs file that *would* get
  diffed into the layer.
- **Distro/runtime detection is best-effort, at `CreateContainer` time.** NRI
  hands the plugin the container's rootfs path before the process execs, so
  the plugin probes cheap marker files (`/etc/debian_version`,
  `/etc/alpine-release`, `/etc/redhat-release`, a `java`/`keytool` binary or
  `/usr/lib/jvm` directory) to decide which of the boot-prepared bundles to
  mount, and where. A detection miss just means that one destination doesn't
  get a mount — it must never fail or block container creation.
- **Environment variables** are appended to `Spec.Process.Env` only (never
  written to any file, never reflected into the built image's config): every
  name/value pair in the sandbox's `proxy-env.json` (decision 3), read fresh
  by the plugin at each `CreateContainer` call.
- **No override.** If the container spec already sets one of these mounts or
  env vars, the plugin leaves it alone — an explicit user choice wins over the
  transparent default.

### 6. The plugin's lifecycle rides `containerd.service`'s existing on-demand activation, not a static boot-time enable

An NRI plugin is a *client* of containerd's NRI socket — it dials out to
register itself — not a listener. Systemd socket activation (a listening
socket that lazily spawns a service on first inbound connection) has no
direct application to a process that only makes outbound connections, so
`discobox-nri-ca.service` cannot be socket-activated in that sense.

The sandbox image already gets the property this ADR actually wants —
incurring no nested-Docker runtime overhead until it's used — for
`containerd.service` and `docker.service` themselves: the Dockerfile disables
both and only enables `docker.socket`, so neither daemon runs until something
first connects to `/var/run/docker.sock`. `discobox-nri-ca.service` piggybacks
on that existing chain instead of introducing a separate activation
mechanism:

- `discobox-nri-ca.service` is **not** enabled at boot (no `WantedBy=`).
- A drop-in on the stock `containerd.service` unit
  (`/etc/systemd/system/containerd.service.d/nri-ca.conf`) adds
  `Upholds=discobox-nri-ca.service` (containerd start pulls the plugin up
  alongside it and restarts it if it dies, without making containerd itself
  depend on the plugin's success) and `ConditionPathExists=/etc/discobox/proxy/mitm-ca.crt`
  on the plugin unit itself, mirroring `discobox-trust-ca.service`'s gating so
  the whole feature is a no-op when no MITM proxy is configured (e.g. local
  development).
- Net effect: the plugin only ever runs when `containerd.service` is actually
  running, and `containerd.service` itself only runs once nested Docker has
  been touched at least once via `docker.socket`'s lazy activation — no
  separate idle-detection logic needed.

### 7. Nested containers reach the pool proxy through a second, bridge-bound `proxy/bridge.Forwarder` instance, not the existing loopback one

`proxy/bridge.Forwarder` is what sandbox processes use as `HTTP_PROXY` today,
listening on loopback (`proxy.SandboxForwarderListen`, `127.0.0.1:17008`).
Nested containers run in Docker's own bridge network namespace inside the
sandbox, so `127.0.0.1` there means the container's own loopback — nothing is
listening on it. The env vars decision 3/5 inject would be unreachable from
inside a nested container without a second listener the container's network
can actually reach.

Widening the existing forwarder to `0.0.0.0` was rejected: `sandbox-agent`'s
image runs under multiple provider backends (local Docker dev, DigitalOcean
VM, the libkrun local VM of ADR 0013) with different network topologies. "It's
fine because this VM has no inbound path" only holds for one of those
backends' network model; it is not a property of the forwarder itself and
must not be relied on.

Instead:

- **Fix the bridge gateway address.** The sandbox's `daemon.json` sets `"bip":
  "172.30.0.1/16"` (a range clear of Docker's own default pool), so the
  gateway a nested container's default route points at is a known constant,
  not something discovered at runtime.
- **A second forwarder instance binds exactly that address**, added as a new
  exported constant next to `proxy.SandboxForwarderListen` (e.g.
  `proxy.NestedForwarderListen = "172.30.0.1:17008"`), reachable only from
  containers on that bridge — never `0.0.0.0`, and no wider than the existing
  loopback instance's trust boundary (the sandbox's own nested containers,
  which the sandbox user already fully controls).
- **It is genuinely socket-activatable**, unlike the NRI plugin: this
  forwarder is a real listener, not a client. A `discobox-proxy-bridge-docker.socket`
  unit sets `ListenStream=172.30.0.1:17008` and `FreeBind=yes` (`IP_FREEBIND`),
  which lets systemd pre-bind that address at boot even though it isn't
  assigned to any interface yet — `docker0` doesn't exist until `dockerd`
  starts. The unit is enabled unconditionally at boot (`WantedBy=sockets.target`);
  holding an unused bound socket costs nothing. The paired
  `discobox-proxy-bridge-docker.service` (`Accept=no`) only starts on the
  first actual connection attempt — which cannot happen before `dockerd` has
  created `docker0` with that exact gateway address and started a container on
  it, so there is no ordering race to manage.
- **`proxy/bridge.Forwarder` gains a second entry point** that serves an
  already-open `net.Listener` (obtained via `coreos/go-systemd/v22/activation`
  from the fd systemd passes it), alongside its existing `ListenAndServe`
  which continues to own the loopback instance unchanged.
- **The NRI plugin substitutes only the URL-valued vars** (`HTTP_PROXY`,
  `HTTPS_PROXY`, `ALL_PROXY`, `NO_PROXY`) with this fixed bridge address when
  injecting into a nested container, rather than the loopback value already in
  `Env`. The file-path-valued vars (`SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`,
  `NODE_EXTRA_CA_CERTS`, ...) pass through unchanged, since those name a mount
  path the plugin makes identical inside and outside the container.
- **Scope**: the default bridge network only, matching decision 5's marker-file
  probing being best-effort. A container joining a custom user-created Docker
  network gets a different gateway address this does not cover.

## Consequences

- Sandbox users' Dockerfiles and `docker run` invocations need no MITM
  awareness; trust is established transparently for every container the
  sandbox's Docker daemon creates, build or run.
- The MITM CA is never present in any layer of any image built inside a
  sandbox — verified by inspecting built image layers, not just asserted from
  the mount/write distinction.
- `docker build`'s execution path inside sandboxes now depends on
  `containerd-snapshotter`, a supported but historically newer dockerd
  feature; this must be re-validated against the pinned `docker-ce` version
  whenever that version is upgraded, since it is the load-bearing assumption
  that unifies build and run under one containerd instance.
- The sandbox image gains a new binary, four systemd units (the NRI plugin,
  not enabled at boot; the extended trust-prep step; a socket-activated
  bridge-bound forwarder pair) and a `containerd.service` drop-in, and
  containerd/dockerd config surface, all owned by `sandbox-agent`.
- The plugin only runs while `containerd.service` is running, which itself
  only runs once nested Docker has been used at least once in that sandbox —
  no persistent daemon overhead in the common case where a sandbox never
  touches Docker. The bridge-bound forwarder is even cheaper: its listening
  socket is pre-bound at boot at effectively no cost, and its process starts
  only on first actual use.
- `proxy/bridge.Forwarder` gains a systemd-activation entry point alongside
  its existing `ListenAndServe`, and the sandbox's `daemon.json` gains a fixed
  `bip`, both net-new surface `proxy`/`sandbox-agent` did not previously own.
- Adding a new proxy-trust env var is a one-line change to `proxy/certs.go`;
  it requires no NRI plugin code change and no `sandboxconfig` schema change,
  since `ProxyEnvs` is derived from that map's keys.
- Adding a new bundle format (e.g. an NSS database) is scoped to
  `discobox-trust-ca.service`'s boot-time prep plus a new marker-file probe in
  the NRI plugin; the plugin's mount logic still never generates a format
  itself per container.
- Detection-based mounting is inherently incomplete: a container built from a
  base image this ADR's marker-file probes don't recognize gets no CA mount,
  and falls back to whatever the env vars alone can cover.

## Deferred

- **Bundle formats beyond PEM copies and Java `cacerts`.** Which additional
  formats (NSS, others) real workloads actually need is not yet known; add
  them to `discobox-trust-ca.service` and the plugin's probe list as concrete
  gaps surface.
- **Per-container opt-out beyond "already set."** No explicit mechanism to
  disable injection for a container that wants isolation from the sandbox's
  MITM proxy entirely, beyond setting its own conflicting env/mount. Revisit
  if that becomes a real use case.
- **Custom Docker networks.** Only the default bridge's fixed gateway address
  is covered. A container joining a user-created network needs its own fixed
  gateway and its own socket-activated forwarder to get the same treatment;
  revisit if nested workloads commonly rely on custom networks.

## References

- [containerd NRI](https://github.com/containerd/nri)
- [Docker containerd image store](https://docs.docker.com/engine/storage/containerd/)
- [OCI runtime spec: hooks and mounts](https://github.com/opencontainers/runtime-spec/blob/main/config.md)
