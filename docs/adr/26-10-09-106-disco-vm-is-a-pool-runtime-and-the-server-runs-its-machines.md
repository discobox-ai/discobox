# 26-10-09-106 — disco-vm is a pool runtime, and the server runs its machines

- **Status**: Accepted (supersedes [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md)
  §2's hypervisor handle in the agent and its helper that serves no API, and
  narrows §1's "Linux sandboxes keep the existing VM-and-container pools": a
  Linux disco-vm sandbox runs on a pool of a remote driver, which is not a host
  pool; supersedes
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
  §2's pool-assembled template, narrows its §1 with an image kind, and narrows
  its §5 for macOS; supersedes [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)'s
  consequence that a provider credential stays in the pool; builds on
  ADR 26-10-09-143, [PR #105](https://github.com/discobox-ai/discobox/pull/105);
  §1's provider type `discovm` and §4's image kind spelling `discovm/<driver>`
  superseded by [26-10-15-629](26-10-15-629-the-vm-provider-and-its-image-kind-are-spelled-vm.md),
  which spells both `vm`)
- **Date**: 2026-10-09
- **Amended**: 2026-10-12, §§1 and 5, the Consequences, one rejected
  alternative and the Status line (which no longer narrows 0144 §3), before
  anything shipped against what they said of boxd (#175).
  As accepted, one Docker-free pool agent served every driver: natively beside
  the server for `vz`, and in a Linux boxd machine for `boxd`. The owner decided
  otherwise: a `boxd` pool's machine runs the ordinary Linux pool agent, and the
  Docker-free agent is for a native macOS or Windows host and nothing else. A
  boxd machine is a whole Linux machine with systemd and root, which is what
  the ordinary agent already runs on, so the second composition bought nothing
  there and made a Linux backend wait on work only a native host needs. §1 now
  says which agent each driver runs and what the boxd pool machine is; §5 no
  longer extends 0144 §§3–6 to a boxd pool. Each boxd sandbox is still a
  machine of its own, driven through §2's seam, and §§2–4 and 6 are as
  accepted.
- **Amended**: 2026-10-15, the Context, §§1–3, 5 and 6, Deferred and the
  Consequences, before anything shipped against them (#224). This ADR was
  written against disco-vm as it stood on 2026-10-09, when the engine reached
  a guest through disco-vm's guest agent and offered `Dial(port)` to any guest
  port. disco-vm's own
  [ADR 0001](https://github.com/discobox-ai/vm/blob/main/docs/adr/0001-a-driver-runs-guest-operations-and-exposes-one-service-endpoint.md)
  has since made guest operations (exec, copy in and out, orderly shutdown,
  info) required methods of each driver's `Machine`, taken `Dial(port)` out of
  the seam, and given an instance one endpoint, for the port its image
  declares as its service. The owner adopted it: the provider places a
  bootstrap with the driver's copy-in, and the address §2's seam returns is
  the instance's endpoint for the agent's port, on vz as a transport the
  server holds, which §3's route is built on rather than on the shim's
  `/dial`; whatever that transport runs through, it never leaves the server.
  disco-vm also gained a `docker` driver; it is recorded as a later `discovm`
  driver, after boxd, and deferred. discobox-ai/vm#7, a boxd endpoint that is
  the same across stop and start, is done and no longer owed. That a Linux
  machine sandbox's guest may already hold the id ADR 0141 would give its
  account (boxd's uid 1000) is noted, and its range deferred, not decided.
  Nothing else changes: boxd is still the first driver, then vz, and
  `discovm` is still beside `dockerworker`, not instead of it.
- **Relates to**: [0006](0006-pool-is-the-runtime-host.md),
  [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md),
  [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  [0099](0099-the-cli-downloads-the-server-it-starts.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  [0141](0141-a-sandbox-account-is-created-with-an-id-the-guest-gives-accounts.md),
  [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md),
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md),
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md),
  and 26-10-09-143 (PR #105).

## Context

ADRs 0144 and 0145 plan macOS and Windows sandboxes as a stack discobox builds
itself: a pool agent that runs as a native host process with no Docker,
Virtualization.framework and Hyper-V lifecycle inside it, and macOS templates
the pool assembles from Apple's restore image. ADR 0126 leaves provider-hosted
sandboxes (exe.dev, Archil) to a backend per provider.

The VM half of that now exists outside discobox. **disco-vm**
([github.com/discobox-ai/vm](https://github.com/discobox-ai/vm)) builds OS images
from a Dockerfile-shaped YAML spec and runs them as VMs: macOS guests on
Virtualization.framework (`vz`), Windows guests on Host Compute Service (`hcs`),
and Linux guests on boxd's cloud microVMs from any host. Its parts that matter
here, as amended on 2026-10-15 to disco-vm's ADR 0001:

- **`pkg/engine` is a library** an embedder opens on a state root: `Create`
  (with a name, a size and guest-to-host `Forwards`), `Start`, `Stop`,
  `Remove`, `Get`/`List`/`State`, and `Endpoint`. Fast clones come from warm
  stages and fall back to cold.
- **Guest operations are the driver's.** Exec, copy in and out, orderly
  shutdown and info are required methods of each driver's `Machine`. vz, hcs
  and docker implement them over a private channel to a guest agent disco-vm
  installs in the image; boxd implements them over its own API, so a boxd
  image carries no disco-vm agent. No raw guest port is part of the seam.
- **An instance has one endpoint.** An image declares one service port
  (`service: {port: N}`), and `Engine.Endpoint` returns that port's one
  address: on boxd, the machine's public HTTPS URL, the same across stop and
  start, with no access control of its own; on vz and hcs, `http://guest` with
  a transport that dials the port, which only the process holding it can use.
- **A local VM is held by a shim process**, which an embedder runs as a
  hidden subcommand of its own binary (`Engine.ShimCommand`, `ShimMain`). The
  shim forwards guest-to-host connections to host Unix sockets, and holds
  the host side of the driver's channel into the guest: on vz the endpoint's
  transport runs through the shim's loopback `/dial`, which, with its token,
  reaches any guest port, the guest agent's included. `Engine.State` is
  whether that shim answers. A remote driver runs no shim.
- **Drivers differ.** `machine.Capabilities` reports guest OS, clone modes,
  `MaxRunning` (two macOS guests per host on vz), display and forwarding. Other
  differences are in the drivers' own documents: hcs needs an elevated process;
  vz uses the framework's NAT for install, boot and save/restore; boxd has no
  guest-to-host forward, is driven by an API key from any host, and does not
  report the org's machine quota.
- **A `docker` driver** (disco-vm's ADR 0002, Proposed) runs Linux guests as
  unprivileged containers that boot systemd in a user namespace of their own.
  It is a remote driver, and its endpoint is a transport, as vz's is.
- **Images are disco-vm's**: a layer's ID is a hash of its parent, driver,
  guest OS, resolved steps and copied files' digests; vendor media (an IPSW, a
  Windows ISO the user names) is fetched on the user's machine and never
  redistributed. An install from `media: latest` is keyed on that string, not
  on the build it resolved to.

Discobox must run disco-vm machines **beside** what it runs today — every
`dockerworker` pool, on every driver — not instead of it.

Much of 0126 has landed: the runtime-supplied dialer (#43), the runtime-config
intake on both sides (#44, #53), live origins over Git HTTP (#45), the worktree
endpoint in the agent (#54), sources the agent clones itself (#55), no pool git
in a checkout and no origin bind (#61), and live certificate renewal (#62). So
have the platform field and placement (#46), `Runtime` owning what `Serve`
calls (#50), host roots (#51), the bridge dialing through `wire` (#52), the
agent's supervision, observation and run-identity seams (#47–#49), and the
overlay manifest (#57). With them, a pool agent whose sandboxes are not
containers is portable Go: nothing it must do for them is Docker. ADR
26-10-09-143 (PR #105) then says how a sandbox with no PID-1 flow boots: its
agent waits for `sandbox.json`, and a VM backend's whole contract is to start
the machine, place the bootstrap, and return one address to the agent's API.

What remains open is where disco-vm sits in that model.

## Decision

### 1. disco-vm is a pool runtime provider, beside dockerworker

The server gains a **`discovm` pool runtime provider**: a
`poolruntime.RuntimeProvider` next to `dockerworker.Engine`, and like it, one
provider with drivers — `vz`, `hcs` and `boxd`, which are disco-vm's; `boxd`
comes first, then `vz`, and disco-vm's `docker` driver is a later one, after
boxd (Deferred) *(amended 2026-10-15)*. A pool on
it is a pool whose sandboxes are disco-vm machines, one per sandbox, named by
its sandbox ID so a lost create is found by name rather than duplicated. A
pool still hosts one platform (0145 §1). No `discovm` pool runs a sandbox as a
container or a sandbox's build on a pool-shared builder, on any driver: each
sandbox is a machine of its own.

Where the pool agent runs, and which agent it is, is the driver's *(amended
2026-10-12)*:

- **A local hypervisor (`vz`, later `hcs`)** — the discobox server runs
  natively on that Mac or Windows machine, and the pool agent runs beside it as
  a native host process, staged and supervised by the provider as 0144 §1 has
  it. This is 0144's Docker-free agent, and a native macOS or Windows host is
  the only place it runs.
- **A remote one (`boxd`)** — the pool agent runs in a Linux boxd machine the
  provider creates for the pool, and its sandboxes are boxd machines of their
  own. That agent is the **ordinary Linux pool agent**, the one a Docker pool's
  container runs, not the Docker-free one: the same binary and entrypoint, as
  root, on the container's state root (`layout.Container()`), with its proxy
  as a systemd unit.

**The boxd pool machine** *(added 2026-10-12)* differs from a Docker pool's
container in these ways:

- **Its image is the pool-agent image's boxd twin**, built from the base
  image's twin as the sandbox images' twins are, with the same agent binary
  at the same path. A machine boots its own init (26-10-09-143), so the agent
  is a unit of the machine's systemd rather than PID 1. The child systemd
  namespace and the PID-1 reaper, which a container's agent has because it is
  PID 1, have no subject there. Nor does the order a container gets from
  that: its agent writes the proxy's environment, CA and control key before
  systemd exists, so in a machine the proxy's unit is ordered after the agent
  has written them.
- **Its bootstrap arrives as a file.** A container is started with its
  bootstrap in its environment; a machine is already running when the
  provider places one in it. It is the same contract, with one addition: the
  pool's runtime, which for a container is always Docker and is nowhere said.
  The agent's unit starts when the file exists, and the agent reads it from
  there. This is 26-10-09-143 §1's shape, which that ADR decides for a
  sandbox's agent only; the file's path and form are the pool image's.
- **It carries nothing a machine sandbox cannot use**: no BuildKit, mediator,
  pool registry or `discobox-pool-runc`, and it needs no Docker daemon. No
  sandbox is a container in it and no sandbox's build reaches it. A Docker the
  guest's base image ships may stay installed, and the twin disables its
  units.
- **Its sandbox runtime is the machine runtime**, whose sandboxes are §2's
  seam instances, in place of the Docker runtime. The bootstrap's runtime says
  which, and a pool runs one kind of image (§4), so an agent never runs both.
  What only the Docker runtime needs — preparing BuildKit, image reclamation,
  the uid-partitioned pool cache — belongs to that runtime and is absent here.
- **It declares the driver's image kind and capacity, not its machine's.**
  The image kind it registers with, and repeats on every report, is the one
  its bootstrap's runtime names (`discovm/boxd`), never the `oci` a container's
  agent declares: registration comes before the seam, and a pool registered
  without its kind would be placed as a Docker pool of its platform. Its
  platform is its own, as a container's agent's is, since its sandboxes are
  machines of the same driver. Its capacity is the driver's limits, learned
  through the seam (§§2, 4). ADR 0071's arithmetic — the pool's services,
  measured in the pool container's own cgroup, plus the sum of its sandboxes'
  own counters, and a walk of the sandboxes' trees — has no subject where each
  sandbox is a machine elsewhere; a sandbox's usage is what its own agent
  reports.

What the `boxd` driver asks of that machine is small: that it is created from
that image and keeps its disk across stop and start, since the agent's
identity key and state live on it; that the provider can place the bootstrap
in it with the driver's copy-in; that it can dial the control plane; and that
its image declares the agent's API as its service, so the machine's endpoint
is that port's HTTPS URL (§3) *(amended 2026-10-15)*. It asks for no Docker
socket, no nested virtualization and no credential of boxd's (§2).

### 2. The server runs the machines; the pool agent decides

The disco-vm engine runs in one place, the server, inside the `discovm`
provider: one image store, one set of shims (a hidden subcommand of the server
binary), and the provider's credential held where every provider's already is
— the boxd API key is provider configuration, as the DigitalOcean token is.

The pool agent stays the runtime authority of ADR 0017 §§9–10. It decides
every create, start, stop and delete, and it alone reports a sandbox's power
state. It asks the provider to carry them out through a **machine seam** served
by the server to that pool only, over the pool's authenticated connection to
the control plane, scoped to the pool's own instances:

- **Per instance**: 26-10-09-143 §2's boot contract — start, place the
  bootstrap with the driver's copy-in, and return the address of the agent's
  port — and the lifecycle this ADR adds around it: create, stop, remove, and
  inspect and list with power state. The seam wraps the engine: the address
  is the instance's endpoint (`Engine.Endpoint`), since the sandbox image
  declares the sandbox agent's port as its service (§3) *(amended
  2026-10-15)*.
- **Per pool**: report the platform and limits the driver offers (§4), and
  build or confirm the sandbox image, reporting progress as the pool's own
  phase (§5).

The provider owns no sandbox intent and publishes no sandbox state: it
executes the pool agent's requests and answers what the hypervisor says. The
engine keeps the instance metadata it needs to find, name and recover its
machines, which is the engine's own bookkeeping, not a second record of the
sandbox. It does serve
an API, which 0144 §2's helper was forbidden to; it holds no pool state,
resolves no secret, and is the one process that can hold a boxd key the pool
agent's own machine must not.

**Power state is the engine's.** A remote driver (`Capabilities.Remote`:
boxd, and disco-vm's docker driver) runs no shim: each engine operation
attaches to the machine and asks its service (boxd's `GetVm`), so a running
machine is observed and reattached across server restarts as it is. A local
machine is held by its shim, and dies with it, so the shim's answer is its
state.

**A local machine lives no longer than its server.** Every VM-backed provider
obeys "the VM dies with the server, the disks do not" (`server/providers` "VM
Lifetime"), and a sandbox VM with no server has no seam, and on vz holds one of
the host's two macOS slots. Each local shim is tied to the server's life the
way the libkrun launcher is — a pipe whose write end the server holds — and the
provider stops any of a pool's local machines it finds running before it
starts that pool's agent. A boxd machine is not the host's to stop: the
provider adopts it again.

### 3. How the pool and its sandboxes reach each other

0126 §5 makes reachability the backend's. The address the seam returns is
built on the instance's endpoint (`Engine.Endpoint`), for the one port its
image declares as its service: the sandbox agent's, or for a boxd pool
machine the pool agent's. The seam offers no other guest port *(amended
2026-10-15)*. Per driver:

- **`vz`.** The endpoint is a transport that dials the agent's port and that
  only the server holds. The address the seam returns is a route the server
  owns, one per instance, built on that transport: it stays the same across
  stop and start and reaches only the sandbox agent's port — 0144 §4's guest
  socket, with the server in between. Only that route is given out. The
  transport, the shim's own loopback port, token and `/dial` beneath it, and
  whatever else of the engine's reaches a guest port — its `Dial` and guest
  client while disco-vm still has them — reach any guest port, and never
  leave the server *(amended 2026-10-15)*. The
  sandbox reaches the pool's proxy, credentials broker and origins as
  guest-to-host forwards (`Forwards`) to Unix sockets the host pool agent
  serves, through `proxy/bridge`'s `wire` scheme (#52). Nothing in that needs a
  network interface (0144 §4), and 0126 §6 requires there be no other route:
  disco-vm's vz driver gives every guest the framework's NAT through install,
  boot and save/restore, so a macOS sandbox is not enabled until disco-vm runs
  one with no route off the machine, warm stages included.
- **`boxd`.** The address is the sandbox machine's endpoint, its HTTPS URL
  for the agent's port (26-10-09-143 §3). The pool agent and the server reach
  each other as DigitalOcean pools do: the pool agent dials the control plane
  at the provider's explicit `controlPlaneUrl`, and the machine seam is served
  on that connection; the server's lease to the pool-agent API is the pool
  machine's endpoint, its HTTPS URL *(amended 2026-10-15)*. A server with no
  URL a boxd machine can reach — one on a laptop — cannot run a boxd pool,
  and the pool agent's API, now on a public URL, has its unauthenticated
  surface audited as 26-10-09-143 §3 has the sandbox agent's. The sandbox's
  way to its pool, and its egress, are what 26-10-09-143 defers, and stay
  deferred: boxd terminates TLS at its
  proxy, so the client certificate 0126 §7 puts on that hop cannot ride it.
  A boxd sandbox is not enabled until its pool hop is mutually authenticated
  and its only egress is the pool's proxy.

### 4. Placement: a platform and an image kind

A pool still hosts one platform, and 0145 §1's placement key stands. Platform
alone no longer says whether a harness can run on a pool: a `boxd` pool and an
amd64 Docker pool are both linux/amd64, and one runs OCI images while the
other runs disco-vm images. So a harness's image declares its **kind** — OCI,
or a disco-vm image for a driver — a pool declares the kind it runs, and a
sandbox is placed only where both match.

A pool's limits are the driver's where it has them. vz's two running macOS
guests (`Capabilities.MaxRunning`) are a limit of the host, not of a pool, and
one provider instance may back several pools (ADR 0003). The server has one
engine per host, and the engine already enforces the cap across every instance
it runs at `Start`; the provider counts running guests across all its vz pools
on that host and closes scheduling on each of them at the cap, so a third
macOS sandbox waits at placement. A create that races past the count is
refused by the engine at start and reported as a capacity wait, not a
failure. boxd's org quota is not reported by disco-vm; until it is, a create
past it fails at the provider and is reported as that failure.

### 5. What it keeps and supersedes

From **0126**, everything: the mount is never the channel, each sandbox owns
its roots, the intake, origins over Git HTTP, reachability per backend, egress
only through the proxy, authenticated pool services, and lifecycle and export
as they are — export of a machine sandbox stays deferred (0144 Deferred),
because disco-vm reads files from a running guest. One Consequence changes: a
provider credential is held by the server's provider, not the pool, because
the pool agent of a remote driver runs on a machine of that provider.

From **0144**:

- **§1 stands** for a local driver: host roots (#51), git on a host (#60) and
  staging and supervising the agent (#64) are still needed. It is **narrowed**
  where it says Linux sandboxes keep the existing VM-and-container pools: a
  Linux sandbox built as a disco-vm image runs on a `discovm` pool of a remote
  driver. That pool is not a host pool: its agent is the ordinary Linux pool
  agent in a machine of the driver's (§1), and nothing of 0144 is extended to
  it *(amended 2026-10-12)*.
- **§2's hypervisor handle in the agent, and its helper that serves no API,
  are superseded** by §2 above. Its authority argument stands. On Windows the
  hcs engine needs the elevation 0144 deferred; that stays deferred with
  Windows.
- **§§3–6 stand, for a local pool**: no Docker services, nothing needing a
  network interface, capacity that describes the machine, and the trust
  posture it states. They say nothing of a `boxd` pool, which §1 above governs
  instead *(amended 2026-10-12)*:
  - **It has no pool-shared builder, registry or `discobox-pool-runc`**, and
    the reason is its own, not 0144 §3's: its sandboxes are machines of their
    own. A Linux disco-vm sandbox image that carries Docker runs it as the
    machine's own, with no pool builder behind it, and its manifest declares
    the pool-shared build path absent. The
    sandbox's own `discobox-runc` and `discobox-docker` (ADR 0020) stay: they
    need no pool service, and they are what makes nested containers trust the
    pool proxy that is their only way out.
  - **Its capacity** is the driver's limits (§4).
  - **Its posture** is a pool container's, in a machine that is the pool's
    alone: the agent runs there as root, as it does in its container, the MITM
    CA is trusted only inside sandboxes, and the boxd key never reaches it
    (§2).

From **0145**:

- **§1 is narrowed** by §4's image kind.
- **§2's assembly by the pool is superseded.** A non-Linux sandbox image is a
  disco-vm image, built by the provider with disco-vm's builder from a
  discobox build spec: the vendor base fetched on the user's machine, then a
  layer that copies in the sandbox-agent release assets and installs them.
  What §2 says of the overlay stands — release assets with a digest each, not
  an OCI artifact — as do building once per machine as its own visible phase,
  and a pin naming the vendor base version and the overlay's digests. The
  layer ID covers the overlay; it does not cover a base installed from
  `media: latest`, so the build records the base version it resolved, and the
  pin names that too.
- **§5 is narrowed for macOS.** A macOS sandbox still has the single account
  the manifest names, but macOS has POSIX ids, so that account has one, given
  under ADR 0141, whose range becomes per guest OS. Windows keeps §5 as
  written.
- **ADR 0141's range is not only a macOS question** *(added 2026-10-15)*. A
  Linux machine sandbox's guest is the driver's own Linux, and an id in it may
  already be taken: on boxd, uid 1000 is boxd's own `boxd` user, which its
  exec runs as, so a `sandbox.json` whose account is uid 1000 fails at
  `usermod` (PR #180). A range per guest OS cannot tell that guest from a
  container sandbox's, which is Linux too. Which ids a Linux machine
  sandbox's account may take is deferred (below), not decided here.
- **§§3, 4, 6, 7 and 8 stand.**

### 6. How it builds on 26-10-09-143

- Its **§1** is how every disco-vm sandbox starts: an image built with the
  sandbox agent as a service of the guest's init, waiting for its bootstrap.
- Its **§2** is the seam's per-instance boot contract, which §2 above wraps
  in create, stop, remove and inspect, and the `discovm` provider is the
  backend it describes. The driver's copy-in places `sandbox.json` before the
  sandbox agent exists; after that, the provider runs nothing in the guest,
  and the pool is given no guest port but the endpoint's *(amended
  2026-10-15)*.
- Its **§3** decides a boxd sandbox's address: the instance's endpoint. Its
  sentence that a local VM's address is 0144's guest socket holds, through the
  endpoint's transport and the server's route on it (§3) *(amended
  2026-10-15)*.

Nothing here changes how a container boots or how a `dockerworker` pool runs.

## Alternatives rejected

- **disco-vm as a sandbox runtime inside an existing Docker pool**, with the
  pool agent in its Linux pool VM and macOS sandboxes relayed to it through the
  host. It keeps one pool agent shape, at the price of a relay for every byte
  between a sandbox and its pool, a pool VM running on every Mac whose only
  sandboxes are macOS, and a pool that hosts several platforms. With the pool
  agent already portable, putting it beside the hypervisor costs less.
- **The pool agent embeds the engine** — 0144 §2 as written, and a boxd pool
  agent calling boxd itself. A pool agent on a boxd machine would then hold a
  key able to operate every machine in the org, on a machine of that org; and
  there would be an engine, an image store and a set of shims per pool rather
  than per server.
- **The Docker-free agent in the boxd machine too**, as this ADR was first
  accepted *(added 2026-10-12)*. It gives every `discovm` pool one agent
  shape. But that composition exists for a host that is the user's own
  machine, where there is no pool container to be PID 1 in and 0144 §6 forbids
  root. A boxd machine is the pool's alone and boots systemd, so an
  unprivileged agent without systemd units answers no constraint of the
  machine's, and it ties a Linux backend to work only a native macOS or
  Windows host needs.
- **The server owns machine sandboxes**, deciding and reporting their state.
  0144 rejected it for splitting the runtime authority, and that still holds:
  the provider here performs the pool agent's requests and reports nothing to
  the control plane on its own.
- **boxd as a `dockerworker` driver**, a boxd machine running Docker with
  sandboxes as containers, as DigitalOcean does. It would work, and it would
  not give a sandbox a machine of its own, which is what a provider-hosted VM
  is for.
- **Discobox's own VM code for sandboxes** — 0145 §2's assembly, a vz and an
  hcs runtime in pool-agent. It duplicates what disco-vm has built and tested
  per driver.

## Deferred

- **Windows (`hcs`).** The driver exists in disco-vm; its elevation, its
  relay, sandbox-agent on windows and a NIC-less run are decided when Windows
  work starts, after boxd and macOS.
- **The `dockerworker` pool VMs on disco-vm.** The vz and libkrun drivers could
  become adapters over `pkg/engine`. Revisited when disco-vm boots Linux guests
  on vz from a released image by digest, with attached volumes.
- **Local machines that outlive their server**, which disco-vm's shims allow;
  revisited if pool VMs ever stop dying with the server.
- **Export and import of a machine sandbox**; revisited when disco-vm can read
  a stopped instance's disk.
- **A boxd sandbox's pool hop and egress**, as 26-10-09-143 defers them.
- **The ids a Linux machine sandbox's account may take** *(added
  2026-10-15)*, given that its guest may already hold some (§5: boxd's uid
  1000); revisited with Linux machine sandboxes' accounts (#121).
- **disco-vm's `docker` driver as a `discovm` driver** *(added 2026-10-15)*:
  Linux machine sandboxes, and their pool's machine, as unprivileged
  containers that boot systemd (disco-vm ADR 0002). It is a remote driver,
  so it runs no shim and its machines are adopted across server restarts as
  boxd's are; but its endpoint is a transport only the server holds, not a
  public URL, so its reachability is vz's — server-owned routes on the
  transport, for the sandbox agent and the pool agent's lease alike — and
  not boxd's. It is a driver of `discovm`, beside
  `dockerworker`; it does not replace `dockerworker`'s sandbox containers.
  Revisited once the boxd driver runs a pool and its sandboxes (#127).

## Consequences

- `server/providers` gains the `discovm` runtime provider with `vz` and `boxd`
  drivers, the machine seam it serves to a pool, and the shim subcommand in
  the server binary. The server module imports `github.com/discobox-ai/vm` by
  commit.
- pool-agent gains a machine runtime whose sandboxes are the seam's
  instances, and two agents run it: the ordinary Linux agent in a boxd pool's
  machine, in place of its Docker runtime, and a Docker-free composition
  (0144 §1's) that runs as a native host process, on darwin and later on
  Windows, and nowhere else. The pool-agent image gains a boxd twin *(amended
  2026-10-12)*.
- A `boxd` pool needs a control plane its machines can reach, and puts the
  pool agent's API on a public URL whose unauthenticated surface is audited.
- A harness's image and a pool declare an image kind, and placement matches
  it with the platform.
- sandbox-agent is released for darwin as assets with digests, and a discobox
  build spec installs them; the pool assembles nothing.
- disco-vm owes discobox, before the driver concerned is enabled: a vz run with
  no route but the forwards, a shim that dies with its embedder, and the base
  version an install resolved *(amended 2026-10-15: a boxd instance's HTTPS
  endpoint, the same across stop and start, is done, discobox-ai/vm#7)*.
- The provider reaches a guest only through the driver's guest operations
  and the instance's endpoint, never a raw guest port or disco-vm's guest
  agent's protocol; where a driver has an agent (vz, hcs, docker), it is that
  driver's own detail *(added 2026-10-15)*.
- ADR 0141's uid range becomes per guest OS. Which ids a Linux machine
  sandbox's account may take is open (§5, Deferred) *(added 2026-10-15)*.
