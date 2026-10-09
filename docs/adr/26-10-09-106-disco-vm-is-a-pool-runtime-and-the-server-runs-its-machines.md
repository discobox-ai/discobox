# 26-10-09-106 — disco-vm is a pool runtime, and the server runs its machines

- **Status**: Accepted (supersedes [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md)
  §2's hypervisor handle in the agent and its helper that serves no API, and
  narrows §1's "Linux sandboxes keep the existing VM-and-container pools" and
  §3's reason with it, by extending 0144's Docker-free pool to a Linux machine
  of a remote driver; supersedes
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
  §2's pool-assembled template, narrows its §1 with an image kind, and narrows
  its §5 for macOS; supersedes [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)'s
  consequence that a provider credential stays in the pool; builds on
  ADR 26-10-09-143, [PR #105](https://github.com/discobox-ai/discobox/pull/105))
- **Date**: 2026-10-09
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
here:

- **`pkg/engine` is a library** an embedder opens on a state root: `Create`
  (with a name, a size and guest-to-host `Forwards`), `Start`, `Stop` (orderly,
  through its guest agent), `Remove`, `Get`/`List`/`State`, `Dial(port)` to any
  guest port, and `Guest(inst).CopyTo` for files. Fast clones come from warm
  stages and fall back to cold.
- **A running VM is held by a shim process**, which an embedder runs as a
  hidden subcommand of its own binary (`Engine.ShimCommand`, `ShimMain`). The
  shim serves `/dial` on loopback, splicing to a guest port, and forwards
  guest-to-host connections to host Unix sockets. `Engine.State` is whether
  that shim answers.
- **Drivers differ.** `machine.Capabilities` reports guest OS, clone modes,
  `MaxRunning` (two macOS guests per host on vz), display and forwarding. Other
  differences are in the drivers' own documents: hcs needs an elevated process;
  vz uses the framework's NAT for install, boot and save/restore; boxd offers no
  socket into a guest (its `Dial` is an authenticated Exec stream), no
  guest-to-host forward, is driven by an API key from any host, and does not
  report the org's machine quota.
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
provider with drivers — `vz`, `hcs` and `boxd`, which are disco-vm's. A pool on
it is a pool whose sandboxes are disco-vm machines, one per sandbox, named by
its sandbox ID so a lost create is found by name rather than duplicated. A
pool still hosts one platform (0145 §1), and its pool agent runs no Docker,
BuildKit, registry or runc wrapper (0144 §3).

Where the pool agent runs is the driver's:

- **A local hypervisor (`vz`, later `hcs`)** — the discobox server runs
  natively on that Mac or Windows machine, and the pool agent runs beside it as
  a native host process, staged and supervised by the provider as 0144 §1 has
  it.
- **A remote one (`boxd`)** — the pool agent runs in a Linux boxd machine the
  provider creates for the pool, as the same portable, Docker-free agent, and
  its sandboxes are boxd machines of their own.

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
  bootstrap, and return the address of the agent's port — and the lifecycle
  this ADR adds around it: create, stop, remove, and inspect and list with
  power state.
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

**Power state is the engine's.** A remote driver (`Capabilities.Remote`,
boxd) runs no shim: each engine operation attaches to the machine and asks
boxd (`GetVm`), so a running boxd machine is observed and reattached across
server restarts as it is. A local machine is held by its shim, and dies with
it, so the shim's answer is its state.

**A local machine lives no longer than its server.** Every VM-backed provider
obeys "the VM dies with the server, the disks do not" (`server/providers` "VM
Lifetime"), and a sandbox VM with no server has no seam, and on vz holds one of
the host's two macOS slots. Each local shim is tied to the server's life the
way the libkrun launcher is — a pipe whose write end the server holds — and the
provider stops any of a pool's local machines it finds running before it
starts that pool's agent. A boxd machine is not the host's to stop: the
provider adopts it again.

### 3. How the pool and its sandboxes reach each other

0126 §5 makes reachability the backend's. Per driver:

- **`vz`.** The address the seam returns is a route the server owns, one per
  instance, that stays the same across stop and start and connects only to
  the sandbox agent's guest port, through the instance's shim — 0144 §4's
  guest socket, with the server in between. The shim's own loopback port,
  token and `/dial`, which reach any guest port and change with every start,
  never leave the server. The
  sandbox reaches the pool's proxy, credentials broker and origins as
  guest-to-host forwards (`Forwards`) to Unix sockets the host pool agent
  serves, through `proxy/bridge`'s `wire` scheme (#52). Nothing in that needs a
  network interface (0144 §4), and 0126 §6 requires there be no other route:
  disco-vm's vz driver gives every guest the framework's NAT through install,
  boot and save/restore, so a macOS sandbox is not enabled until disco-vm runs
  one with no route off the machine, warm stages included.
- **`boxd`.** The address is the sandbox machine's HTTPS URL for the agent's
  port (26-10-09-143 §3). The pool agent and the server reach each other as a
  DigitalOcean pools do: the pool agent dials the control plane at the
  provider's explicit `controlPlaneUrl`, and the machine seam is served on that
  connection; the server's lease to the pool-agent API is the pool machine's
  HTTPS URL. A server with no URL a boxd machine can reach — one on a laptop —
  cannot run a boxd pool, and the pool agent's API, now on a public URL, has
  its unauthenticated surface audited as 26-10-09-143 §3 has the sandbox
  agent's. The sandbox's way to its pool, and its egress, are
  what 26-10-09-143 defers, and stay deferred: boxd terminates TLS at its
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
  Linux sandbox built as a disco-vm image runs on a `discovm` pool, whose agent
  is the same Docker-free composition in a Linux machine of the driver's.
- **§2's hypervisor handle in the agent, and its helper that serves no API,
  are superseded** by §2 above. Its authority argument stands. On Windows the
  hcs engine needs the elevation 0144 deferred; that stays deferred with
  Windows.
- **§§3–6 stand** for a local pool: no Docker services, nothing needing a
  network interface, capacity that describes the machine, and the trust
  posture it states. For a `boxd` pool:
  - **§3's absences hold**, and its reason changes: a `discovm` pool has no
    pool-shared builder, registry or `discobox-pool-runc` because its
    sandboxes are machines of their own. A Linux disco-vm sandbox image that
    carries Docker runs it as the machine's own, with no pool builder behind
    it, and its manifest declares the pool-shared build path absent. The
    sandbox's own `discobox-runc` and `discobox-docker` (ADR 0020) stay: they
    need no pool service, and they are what makes nested containers trust the
    pool proxy that is their only way out.
  - **§5's capacity** is the driver's limits (§4).
  - **§6's posture** is the pool machine's: it is the pool's alone, the agent
    runs there as a dedicated unprivileged user — not root, so `layout.Host`
    serves it as it does a Mac's — and the MITM CA is trusted only inside
    sandboxes.

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
- **§§3, 4, 6, 7 and 8 stand.**

### 6. How it builds on 26-10-09-143

- Its **§1** is how every disco-vm sandbox starts: an image built with the
  sandbox agent as a service of the guest's init, waiting for its bootstrap.
- Its **§2** is the seam's per-instance boot contract, which §2 above wraps
  in create, stop, remove and inspect, and the `discovm` provider is the
  backend it describes. disco-vm's own guest agent places
  `sandbox.json` (`CopyTo`) before the sandbox agent exists; after that,
  nothing uses it, and the pool is given no other guest port.
- Its **§3** decides a boxd sandbox's address. Its sentence that a local VM's
  address is 0144's guest socket holds, through the shim.

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

## Consequences

- `server/providers` gains the `discovm` runtime provider with `vz` and `boxd`
  drivers, the machine seam it serves to a pool, and the shim subcommand in
  the server binary. The server module imports `github.com/discobox-ai/vm` by
  commit.
- pool-agent gains a Docker-free composition (0144 §1's) that runs as a host
  process on darwin and as a process in a Linux boxd machine, and a machine
  runtime whose sandboxes are the seam's instances.
- A `boxd` pool needs a control plane its machines can reach, and puts the
  pool agent's API on a public URL whose unauthenticated surface is audited.
- A harness's image and a pool declare an image kind, and placement matches
  it with the platform.
- sandbox-agent is released for darwin as assets with digests, and a discobox
  build spec installs them; the pool assembles nothing.
- disco-vm owes discobox, before the driver concerned is enabled: a vz run with
  no route but the forwards, a shim that dies with its embedder, the base
  version an install resolved, and an HTTPS address for a boxd instance's
  port.
- ADR 0141's uid range becomes per guest OS.
