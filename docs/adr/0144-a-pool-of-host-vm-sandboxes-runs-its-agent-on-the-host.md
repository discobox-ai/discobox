# 0144 — A pool of host-VM sandboxes runs its agent on the host

- **Status**: Accepted
- **Date**: 2026-09-23
- **Relates to**: [0006](0006-pool-is-the-runtime-host.md),
  [0013](0013-local-linux-pools-use-libkrun-microvms.md),
  [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md),
  [0044](0044-builds-run-on-a-pool-shared-buildkit.md),
  [0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md),
  [0071](0071-resource-accounting-is-a-pool-agent-differenced-report.md),
  [0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md), and
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md).
  It narrows 0006's "the pool host is a Docker host" for one pool kind; every
  existing pool keeps the current shape.

## Context

Every pool today ends the same way: `dockerworker.Engine` runs the pool-agent
container on a Docker daemon, and each driver differs only in VM lifecycle, how
to reach that daemon, and which guest image it boots. The pool agent is PID 1
in that container, forks a systemd child namespace, and creates its sandboxes
as sibling containers on the same daemon.

We want a macOS or Windows sandbox on the user's own machine: a guest VM under
Virtualization.framework or Hyper-V. It is not a container, it shares no
filesystem with the pool, and — decisively — nothing inside a Linux guest can
create it. Keeping the current shape would boot a Linux VM whose only job is to
hold the agent, and that agent would then have to ask the host to create each
sibling VM and relay every byte to it, because guest sockets reach the host and
not a sibling.

Most of what makes the pool agent Linux is what such a pool has no use for.
BuildKit, its mediator, the pool registry and `discobox-pool-runc` exist to
intercept `docker build` in a Linux sandbox (ADR 0044, 0047, 0050); a macOS or
Windows sandbox runs no nested Docker to intercept. Image reclamation assumes
sandboxes come from OCI images. The uid-partitioned pool cache (ADR 0094) has
no subject once each sandbox's cache is private (ADR 0126 §2). The systemd
child namespace and the PID-1 reaper (ADR 0087) exist because the agent is
PID 1 in a container.

What is left is portable Go: identity, registration and `poolauth`; the
pool-local API with its scopes, autostart latch, status poll and audit relay;
Git origins; and the proxy with its credential broker and sentinel resolution.
The root `proxy` package carries no build tags and no `syscall`, `/proc`,
netns or cgroup code, and the sqlite under its audit store is the pure-Go
driver, so it builds and links off Linux without cgo.

Both platforms also offer a host-to-guest socket, which is neither a network
nor a shared filesystem: `AF_VSOCK` in a macOS 13 or later guest whose VM
configuration carries a virtio socket device, and Hyper-V sockets (`AF_HYPERV`,
with a service GUID registered under `GuestCommunicationServices`) in a Windows
guest. A host process can reach such a guest with no NIC, no inbound port, and
nothing shared.

## Decision

### 1. The pool agent runs as a host process

A pool whose sandboxes are host VMs runs the pool-agent binary natively on that
machine. There is no pool VM, no pool container, and no Docker daemon in the
pool. The binary is staged and supervised the way a server binary already is
([0099](0099-the-cli-downloads-the-server-it-starts.md)), and its durable state
— identity key, Git origins, proxy material, audit database — lives under a
host path. `layout` gains host roots per OS; `ContainerRoot` describes the
container's view and is not one of them.

A pool hosts one platform. A host pool refuses a sandbox whose image is not its
platform, rather than degrading into something that boots and then cannot run
the harness. Linux sandboxes keep the existing VM-and-container pools, and a
user who wants both has two pools — which the model already allows, since a
pool binds immutably to one provider instance (ADR 0003, 0006).

### 2. The pool agent is still the sandbox runtime authority

`sandboxruntime.Runtime` gains a host-VM implementation beside
`DockerSandboxRuntime`, and everything above that interface is unchanged: the
pool-local API is still the only way the server and CLI reach a sandbox, with
the same routes, scopes, autostart latch, archive and delete confirmations, and
the same state channel. ADR 0017 §§9–10 stand as written — the pool agent is
still the component that can see whether a sandbox is running, because it is
now the process holding the hypervisor handle.

This is the whole reason the agent runs on the host rather than beside it.
Anything that put VM lifecycle in the server would split the runtime authority
in two and make power state arrive on one channel for some sandboxes and
another for the rest.

Where the platform requires privilege for VM lifecycle, a minimal helper
performs VM CRUD on the agent's behalf and nothing else: it holds no pool
state, resolves no secret, and serves no API. The agent remains the authority;
the helper is a syscall with a privilege boundary in front of it.

### 3. A host pool does not run what its sandboxes cannot use

BuildKit, the mediator, the pool registry, `discobox-pool-runc`, image
reclamation, the nested-Docker trust injection, the uid-partitioned cache, and
the systemd child namespace are absent from a host pool. This is a property of
the pool's platform, not a stage of implementation: a sandbox with no nested
Docker has no build to hand to a shared builder, and ADR 0126 §2 already makes
each sandbox's cache its own. Nothing here is to be revisited "when there is
time"; it comes back only if such a pool ever hosts Linux sandboxes, which §1
forbids.

### 4. Nothing a sandbox needs requires a network interface

The agent dials each sandbox's guest socket, and that socket carries everything
the sandbox and the pool say to each other: the pool-local API, the proxy hop,
the credentials broker, and Git. No part of the contract is expressed as an
address, a port, or a name to resolve, so a sandbox with no network interface
at all is fully functional — which is what makes the Docker pools' `Internal`
property reachable here without a network to make internal.

**Whether an interface exists is the driver's decision**, not this contract's. A
driver may attach one for its own reasons; what it may not do is give the
sandbox a route the proxy does not own (ADR 0126 §6). The contract's job is to
need nothing from it either way.

The in-sandbox half is unchanged: `proxy/bridge` still listens on plaintext
loopback inside the sandbox and forwards to the pool proxy. Only its dial
target changes, and it changes where every other transport in this system is
chosen — `wire`, from a URL scheme. The proxy keeps an ordinary listener and
learns nothing about hypervisors.

mTLS survives the move even though the socket already identifies the VM. The
client certificate's common name is the proxy's tenant identity, so keeping it
means the proxy is unchanged; the socket's identity is a second check that a
stolen certificate does not satisfy.

### 5. Capacity and accounting describe a machine, not a sized VM

A host pool's `ready`, `schedulable` and `degraded` and its CPU, memory and
storage describe the user's machine. ADR 0071's arithmetic does not survive the
move: pool services are ordinary host processes, a sandbox is a VM whose usage
is reported by the guest agent or the hypervisor, and no cgroup contains
either, so "the pool's load is services plus the sum of its sandboxes" is two
measurements taken by different instruments. Per-sandbox disk is the VM's own
disk as the hypervisor reports it, not a tree walk the agent performs. The
create phases lose the image pull and gain the VM's own (ADR 0060 still governs
what a phase is).

### 6. The trust posture changes, and is stated rather than assumed

In a Docker pool the MITM CA key, the sentinel registry, the Git origins and
the audit database sit inside the pool VM, and the host sees a VM. In a host
pool they are files on the user's own machine, owned by the user the agent runs
as, and the proxy is host-native code rather than code confined to a guest.

Three rules follow. The agent runs as the user, never as root or an
administrator — the privileged helper of §2 is the only exception and does
nothing else. The MITM CA is never added to the host's own trust store; it is
trusted inside sandboxes only. And the sandbox VM, not the pool process, is the
isolation boundary for anything a sandbox runs.

## Alternatives rejected

- **Boot a Linux VM whose only job is to host the agent.** It keeps today's
  pool shape and nothing else: a guest image to pull, a VM to size and boot,
  and a host-side relay for every byte between the agent and a sibling VM,
  because a guest socket reaches the host rather than a sibling. The agent
  still cannot create a VM from inside a guest, so sandbox lifecycle would
  have to travel back out to the host regardless. What it buys is the
  confinement of pool state that §6 gives up deliberately — and it buys that
  for the pool's own secrets while leaving the sandbox VMs, which hold the
  user's work, exactly where they are.
- **Let the server create and observe the sandbox VMs, and have the agent
  adopt them.** The server already drives hypervisors for pools, so this looks
  like the smaller change. It splits the runtime authority: an adoption
  handshake to keep the two in step, power state published by the server for
  one kind of sandbox and by the agent for every other, and two components
  that both believe they know which sandboxes exist. ADR 0017 §10 exists
  because the observer should be the reporter, and on a host pool the agent is
  an observer.
- **Fold the pool into the server for host pools.** Rejected for the reason
  ADR 0126 gives: it displaces the proxy, the origins, the pool-local API and
  status reporting, and it would make a host pool a different API surface from
  every other pool for the CLI and the server alike.
- **Keep a Linux builder beside the host pool so `docker build` still works.**
  Nothing in a macOS or Windows sandbox produces a Linux build for it to serve.
  A pool-shared builder is a Linux-sandbox feature, and ADR 0044's argument for
  it does not survive the platform change.
- **Reach the sandboxes over a host-only network instead of a guest socket.**
  It has no route off the machine either, but it costs address assignment, a
  guest listener any process on the host can reach, and a per-OS firewall
  assumption to make "host-only" true. A guest socket is point-to-point, is
  identified by the hypervisor, and needs none of that.

## Consequences

- A host pool is a new pool runtime shape. Everything that assumed "every
  backend ends with a pool-agent container on a Docker daemon" — the engine,
  the drivers' connection leases, pool logs, repair and drift detection — has
  to account for a supervised host process instead.
- The pool-agent binary joins the release artifacts that are signed per
  platform, and on macOS it needs the virtualization entitlement the server's
  driver already carries.
- `git http-backend` needs a `git` on the host. macOS has one only with the
  Command Line Tools installed and Windows ships none, so a host pool either
  stages a binary or serves its bare origins without one.
- Hyper-V lifecycle requires elevation, which is what §2's helper is for; a
  macOS pool's size is capped by Apple's licence limit on concurrent macOS
  guests per host, whatever the machine could otherwise run.
- ADR 0126 gets smaller for this case. With the agent on the host there is no
  cross-machine hop and no relay, and its outbound session reduces to dialing a
  guest socket. 0126 remains the decision for provider-hosted remote sandboxes.
- Platform becomes a property the control plane places on, which the non-Linux
  sandbox decision has to define; this ADR assumes it exists and does not
  define it.

## Deferred

- **The privileged helper's shape on Windows.** Decided when the Windows guest
  work starts, and revisited if Hyper-V lifecycle can be driven without
  elevation; on macOS no helper is needed.
- **Sharing anything between a host pool's sandboxes.** Caches and
  source-scoped data stay private per sandbox, as in ADR 0126 §2. Revisit only
  if a real workload needs sharing and the platform offers an isolated way to
  provide it.
- **Export and import for a host-VM sandbox.** ADR 0126 §8's rule holds — the
  tree is read from a stopped disk — and how a hypervisor presents that disk is
  settled when transfer is wanted for these sandboxes.
