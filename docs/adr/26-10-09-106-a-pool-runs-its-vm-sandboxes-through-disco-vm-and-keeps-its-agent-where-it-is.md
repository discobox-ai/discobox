# 26-10-09-106 — A pool runs its VM sandboxes through disco-vm, and keeps its agent where it is

- **Status**: Proposed (on acceptance, supersedes [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md)
  §§1, 3, 5 and 6 and §2's hypervisor handle in the agent and its
  helper that serves no API, and narrows its §4; supersedes [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
  §1's one platform per pool and §2's pool-assembled template, and narrows
  its §5 for macOS; builds on
  ADR 26-10-09-143, [PR #105](https://github.com/discobox-ai/discobox/pull/105))
- **Date**: 2026-10-09
- **Relates to**: [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md),
  [0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md),
  [0101](0101-one-guest-image-for-every-vm-backend-and-the-kernel-is-separate.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  [0141](0141-a-sandbox-account-is-created-with-an-id-the-guest-gives-accounts.md),
  [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md),
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md),
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md),
  and 26-10-09-143 (PR #105).

## Context

ADRs 0144 and 0145 plan macOS and Windows sandboxes as a stack discobox builds
itself: a pool agent that runs as a native host process with a reduced set of
services, VM lifecycle on Virtualization.framework and Hyper-V inside it, and
macOS templates the pool assembles from Apple's restore image. ADR 0126 leaves
provider-hosted sandboxes (exe.dev, Archil) to a backend per provider.

That machinery now exists outside discobox. **disco-vm**
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

Discobox must run disco-vm machines **beside** what it runs today — the Docker
pools on every provider, and the vz, libkrun and wslc pool VMs — not instead of
them.

Much of 0126 has landed on the Docker pool: the runtime-supplied dialer (#43),
the runtime-config intake on both sides (#44, #53), live origins over Git HTTP
(#45), the worktree endpoint in the agent (#54), sources the agent clones
itself (#55), no pool git in a checkout and no origin bind (#61), and live
certificate renewal (#62). The platform field and placement (#46), the agent's
supervision, observation and run-identity seams (#47–#49), and the overlay
manifest (#57) have too. ADR 26-10-09-143 (PR #105) then says how a sandbox
that has no PID-1 flow boots: its agent waits for `sandbox.json`, and a VM
backend's whole contract is to start the machine, place the bootstrap, and
return one address to the agent's API.

What remains open is where disco-vm sits in that model.

## Decision

### 1. disco-vm is a sandbox runtime of the pool

A pool runs a disco-vm sandbox the way it runs a container: through
`sandboxruntime.Runtime`, which gains a **machine runtime** beside the Docker
one. One sandbox is one disco-vm instance, named by its sandbox ID, so a lost
create is found by name rather than duplicated. The pool agent stays the
runtime authority of ADR 0017 §§9–10: it decides every create, start, stop and
delete, and it is the only component that reports a sandbox's power state.

disco-vm is not a pool provider and not a pool kind. A pool provider makes the
place a pool agent, its proxy, its origins and its builder run; a disco-vm
instance is a sandbox in such a place. The pool agent keeps running where it
runs today — in its container, in its Linux pool — with every service it has.

### 2. The engine runs where its driver reaches its hypervisor

The machine runtime talks to a **machine seam** with the operations of
26-10-09-143 §2 and nothing else: create, start, stop, remove, inspect and
list (power state included), place the bootstrap, and connect to the agent's
port. Two things serve that seam, because two kinds of driver exist:

- **A local hypervisor** (vz on a Mac, hcs on Windows) cannot be reached from
  inside a Linux pool VM. Its engine runs on the host, in the discobox server,
  as part of the pool's own provider — the vz provider on a Mac, the wslc
  provider on Windows — which already runs that pool's VM and holds the
  virtualization entitlement. The provider serves the seam **to that pool's
  agent only**, on a channel of its own into the pool VM — on vz, a new
  guest-to-host port in the pool VM's port map, accepted by the provider
  rather than by the control plane's `carrierhub` — carrying both the seam's
  requests and the raw byte streams it connects. Its shims are a hidden
  subcommand of the server binary.
- **A remote driver** (boxd) is an API reachable from anywhere. Its engine runs
  in the pool agent, as a library, so the provider credential and the image
  store stay in the pool's durable tree, where 0126's Consequences put them.
  The pool-agent binary carries the shim subcommand, and the shim gets the key
  from the pool, never from the environment of an unrelated process.

The provider decides nothing and records nothing about a sandbox: it executes
the pool agent's requests and answers what the hypervisor says. The authority
0144 §2 argued for stays with the agent; the hypervisor handle moves to the
process that can hold it. That process does serve an API — the seam — and
relays its sandboxes' traffic, which 0144 §2's helper was forbidden to do; it
still holds no pool state and resolves no secret, and the bytes it relays are
mTLS between the sandbox and the pool (0126 §7), opaque to it.

**Power state comes from the driver, not from a shim.** `Engine.State` today
reports a machine whose shim has gone as stopped, which on boxd is a machine
still running and billing. The seam reports what the hypervisor or the
provider's API says, and adopts a running machine whose shim has gone; disco-vm
gains that before a remote driver is enabled.

**A machine sandbox lives no longer than its pool.** Every VM-backed provider
obeys "the VM dies with the server, the disks do not" (`server/providers`
"VM Lifetime"), and a sandbox VM with no pool has no proxy, no agent watching
it, and on vz holds one of the host's two macOS slots. So a local provider
stops its pool's sandbox VMs, in order, when it stops the pool VM; and because
a server that is killed stops nothing, each shim is tied to the server's life
the way the libkrun launcher is — a pipe whose write end the server holds, so
the shim powers its VM off when the server process exits however it exits —
and the provider stops any sandbox VM of its pool it finds running before it
boots the pool VM. The pool agent re-observes its sandboxes when the pool
comes back. A boxd machine is not the host's to stop: the pool agent re-adopts
it on restart.

### 3. A pool hosts targets, and a sandbox is placed on one

Platform alone cannot place a machine sandbox: a boxd guest is linux/amd64, the
same platform as an amd64 pool's containers. A pool therefore reports the
**targets** it hosts, each a platform and a runtime — its containers, and each
machine driver its seam offers with that driver's guest platforms. A harness's
image says what it is — an OCI image, or a disco-vm image for a driver — and a
sandbox is placed on a pool that hosts a matching target. The sandbox's
platform and runtime are the matched target's, recorded at placement and
immutable after, rather than copied from the pool.

A target carries its own limits where the driver has them: vz's two running
macOS guests come from `Capabilities.MaxRunning` and reach placement as a count
per target, so a third macOS sandbox is refused before it starts. boxd's org
quota is not reported by disco-vm; until it is, a create past it fails at the
provider and is reported as that failure.

### 4. What stays from 0126

All of it. The mount is never the channel (§1), each sandbox owns its roots
(§2), config, secrets and readiness converge on the intake (§3), the origin is
reached over Git HTTP (§4), reachability is the backend's (§5), egress is the
proxy's alone (§6), pool services are reached over authenticated paths (§7),
and lifecycle and export keep their meanings (§8). The work that made it true
on the Docker pool is the same work a machine sandbox relies on.

What a machine backend owes 0126, per driver:

- **§5 reachability.** On a local hypervisor the address is the guest socket,
  reached through the provider's channel of §2 and the instance's shim: 0126
  §5's "address the pool can dial", one relay longer than 0144 planned. On boxd
  it is the provider's HTTPS URL, as 26-10-09-143 §3 decides.
- **§6 egress.** A local machine sandbox reaches the pool's proxy, credentials
  broker and origins as guest-to-host forwards (`Forwards`) that the provider
  relays into the pool VM on a new host-to-guest port; none of today's (pool
  API, shutdown, Docker socket) reaches them. Its only route off the machine
  must be that relay. disco-vm's vz driver gives every guest the framework's
  NAT, through install, boot and save/restore alike, and hcs gives each VM a
  NIC, so neither meets §6 today. A local machine sandbox is not enabled on a
  driver until disco-vm can run it with no other route — a VM with no NIC, or
  a NIC the host filters to nothing — warm stages included. boxd's egress is
  deferred by 26-10-09-143 and stays so.
- **§8 lifecycle.** Stop is disco-vm's orderly shutdown, power state is §2's,
  and a lost connection is not a stop. Clear-cache clears the sandbox's private
  cache through the agent. Export and import of a machine sandbox stay deferred
  (0144 Deferred): disco-vm reads files from a running guest, and 0126 §8 needs
  a stopped one.

### 5. What it supersedes in 0144 and 0145

From **0144**:

- **§1, the host-process pool agent, is superseded.** There is no host pool,
  no host roots, no pool binary staged and supervised beside the server.
- **§2's placement of the hypervisor handle in the agent, and its helper that
  serves no API, are superseded** by §2 above. Its authority argument stands.
  On Windows the hcs engine needs the elevation whose helper 0144 deferred
  deciding; that helper is still only VM CRUD, beneath the provider.
- **§3 no longer applies.** The pool keeps BuildKit, the registry and its other
  Linux services, which its Linux sandboxes use; a machine sandbox does not
  reach them, as 0145 §7 already declares.
- **§4 is narrowed.** "Nothing a sandbox needs requires a network interface"
  stands as the obligation of §4 above; "the agent dials each sandbox's guest
  socket" becomes the provider's relay.
- **§5 is superseded.** A pool's capacity and accounting stay ADR 0071's for
  its own VM; a machine target's limits are §3's.
- **§6 is superseded.** The trust posture does not change: the MITM CA key,
  sentinels, origins and audit stay inside the pool VM.

From **0145**:

- **§1's "a pool hosts exactly one platform" is superseded** by §3's targets.
  The placement key, and a refusal at placement on mismatch, stand.
- **§2's assembly by the pool is superseded.** A non-Linux template is a
  disco-vm image, built by disco-vm's builder from a discobox build spec: the
  vendor base fetched on the user's machine, then a layer that copies in the
  sandbox-agent release assets and installs them. What §2 says of the overlay
  stands — release assets with a digest each, not an OCI artifact — as do
  building once per machine as its own visible phase, and a pin that names
  both the vendor base version and the overlay's digests. The layer ID covers
  the overlay; it does not cover a base installed from `media: latest`, so the
  build records the base version it resolved, and the pin names that too.
- **§5 is narrowed for macOS.** A sandbox there still has the single account
  the manifest names, but macOS has POSIX ids, so that account has one, given
  under ADR 0141. Windows keeps §5 as written.
- **§§3, 4, 6, 7 and 8 stand**: the manifest, the API that does not fork,
  platform paths, trust and absences, and transfers that stay on their
  platform.

### 6. How it builds on 26-10-09-143

26-10-09-143 is the boot contract this relies on, and this decision supplies
its backend:

- Its **§1** is how every machine sandbox starts: an image built with the
  sandbox agent as a service of the guest's init, waiting for its bootstrap.
- Its **§2** is the machine seam's whole surface. disco-vm's own guest agent
  places `sandbox.json` (`CopyTo`) before the sandbox agent exists; after that,
  nothing in the pool uses it, and the pool exposes no other guest port.
- Its **§3** decides boxd's address. Its sentence that a local VM's address is
  0144's guest socket still holds: it is that socket, reached through the
  provider rather than from a pool agent on the host.

Nothing here changes how a container boots.

## Alternatives rejected

- **Keep 0144's host pool agent, with disco-vm under it.** It is the other
  coherent placement: the agent beside the hypervisor, with no relay. It still
  costs a second composition of the pool agent, the proxy, origins and audit
  store as host processes on darwin and windows, `git` on a host that has none
  (#60), a pool binary staged and supervised per platform (#64), host roots,
  and the trust posture 0144 §6 had to state — all to save one relay disco-vm's
  shim already provides. 0144 rejected a Linux VM "whose only job is to host
  the agent"; on a machine that runs discobox, that VM is the pool its Linux
  sandboxes already need.
- **disco-vm as a pool provider**, a pool whose agent runs in a disco-vm guest.
  For a macOS or Windows guest that is the port of the pool agent to darwin and
  windows this decision avoids, and the agent still could not create its
  sibling sandboxes from inside a guest, so their lifecycle would come back out
  to the host — the relay chosen here, paid for twice. For a Linux guest it is
  today's pool VM on another hypervisor, which the Deferred list covers.
- **The server owns machine sandboxes**, and the pool adopts them. 0144
  rejected it for splitting the runtime authority, and that still holds: the
  provider here performs operations and decides and reports nothing.
- **Every engine on the host, boxd included.** One placement for every driver,
  but it moves the provider's credential out of the pool, and a provider-hosted
  sandbox would need the user's machine up to be started.
- **Place on platform alone.** It cannot tell a boxd guest from an amd64 pool's
  container, and a sandbox would land in whichever runtime the pool tried
  first.
- **Discobox's own VM code for sandboxes** — 0145 §2's assembly, a vz and an
  hcs runtime in pool-agent. It duplicates what disco-vm has built and tested
  per driver.

## Deferred

- **The pool VMs themselves on disco-vm.** The vz and libkrun providers could
  become adapters over `pkg/engine`, as disco-vm's own plan has it. Revisited
  when disco-vm boots Linux guests on vz from a released image by digest, with
  attached volumes; nothing here depends on it.
- **Machine sandboxes that outlive their pool VM.** disco-vm's shims allow it;
  revisited if the pool VM ever stops dying with the server.
- **Export and import of a machine sandbox**, as 0144 deferred it; revisited
  when disco-vm can read a stopped instance's disk.
- **Egress on boxd and reaching the pool from a provider VM**, as
  26-10-09-143 defers them.

## Consequences

- `sandboxruntime` gains a machine runtime and a machine seam with two
  implementations: the provider's, over a new channel into the pool VM, and an
  in-process engine for remote drivers.
- The pool VM's vsock port map gains a guest-to-host port for the seam and a
  host-to-guest port for the sandbox relay: a coordinated release of the vm
  image (ADR 0101).
- The server module imports disco-vm's `pkg/engine` and drivers, and the server
  binary carries the shim subcommand; so does pool-agent, for boxd. On Windows
  the hcs engine needs an elevated helper.
- A pool reports targets rather than one platform, and a sandbox records the
  target it was placed on; placement, the pool's status, and the server's
  model and its migration follow.
- sandbox-agent is released for darwin and windows as assets with digests, and
  a discobox build spec installs them; the pool assembles nothing.
- A Mac that runs macOS sandboxes also runs its Linux pool VM.
- disco-vm owes discobox, before the driver concerned is enabled: a run with no
  route but the forwards (vz, hcs), a shim that dies with its embedder (vz,
  hcs), power state from the driver and adoption of a running machine (boxd),
  the base version an install resolved, and an HTTPS address for a boxd
  instance's port.
- ADR 0141's uid range is Debian's; it is settled per guest OS before a macOS
  sandbox's account is created (§5's narrowing of 0145 §5).
