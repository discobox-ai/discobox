# 0013 — Local Linux pools use libkrun microVMs with VSOCK and passt

- **Status**: Accepted
- **Date**: 2026-07-23

## Context

The local Docker provider runs every pool on the developer's host Docker
daemon. The DigitalOcean provider gives each pool a VM, but reaches Docker over
SSH and the pool-agent over an IP listener. A local Linux VM provider should
keep the existing one-VM-per-pool and `dockerworker.Driver` architecture while
providing a stronger boundary than a host container.

The local VM has stricter transport requirements than the cloud driver:

- it must not create a TAP, TUN, veth, bridge, or host network namespace;
- it must not publish or forward a TCP or UDP port from the guest;
- guest outbound traffic must leave through unprivileged user-mode networking;
- both control-plane-initiated and pool-agent-initiated HTTP must cross the VM
  boundary over VSOCK;
- its immutable root filesystem must be built from a Dockerfile; and
- durable data and disposable cache must be independently attachable.

QEMU can meet those requirements, but it brings a generic machine emulator and
a large legacy device surface to a deliberately narrow workload. Cloud
Hypervisor and Firecracker are smaller command-line VMMs, but their normal
network paths require a TAP device. libkrun is a library VMM assembled from
rust-vmm, Firecracker, and Cloud Hypervisor components. It intentionally
implements a small device model rather than a general-purpose VM platform. Its
stable 1.x API directly supports KVM, multiple raw or QCOW2 virtio-block
devices, VSOCK-to-Unix-socket redirection, and a virtio-net Unix-stream backend
for passt.

libkrun also offers Transparent Socket Impersonation (TSI), which provides
guest networking without any network interface or separate network process.
The bundled guest kernel implements TSI by replacing every userspace
`AF_INET`/`AF_INET6` stream or datagram socket with a VSOCK-backed proxy
socket. That includes applications inside Docker network namespaces. Discobox
currently relies on an internal Docker bridge to make the pool proxy the only
egress path from sandbox containers, so global TSI interception would bypass
the boundary that enforces that topology.

The existing provider architecture also matters: `dockerworker.Engine` owns
the pool-agent container and all Docker mechanics. A backend supplies VM CRUD,
a Docker client lease, and a pool-agent client lease. Moving the pool-agent
directly into one provider's guest image would fork that lifecycle.

## Decision

### 1. The local Linux backend is a libkrun `dockerworker.Driver`

Add a local Linux VM driver beneath `dockerworker.Engine`. One pool is one
libkrun microVM and one guest Docker daemon. The engine continues to launch and
replace the pool-agent container exactly as it does for DigitalOcean.

The initial platform is x86-64 Linux with usable `/dev/kvm`. The driver fails
validation when KVM is unavailable; it does not silently fall back to software
emulation.

libkrun is embedded in a small `discobox-krun` launcher executable, not linked
into the long-lived Go server. `krun_start_enter` consumes its calling process
and exits that process when the guest exits. Keeping that call in a dedicated
executable isolates libkrun's native ABI and future major-version changes from
the server process.

The launcher:

- attaches the root, data, and cache disks with explicit formats and
  read-only flags;
- selects the root block device and starts `/sbin/init`;
- connects one virtio-net device to a per-VM passt Unix socket;
- disables libkrun's implicit VSOCK device and adds one explicit VSOCK device
  with a TSI feature mask of zero; and
- configures only the VSOCK port-to-Unix-socket mappings required by
  Discobox.

The backend does not use `krunvm`; its OCI-image-oriented lifecycle does not
model Discobox's three persistent block devices or bidirectional control
channels.

The launcher is a pool runtime, not an attached subprocess whose lifetime is
coupled to the server:

- `discobox-server` starts `discobox-krun` in its own process session, with
  direct log files and no parent-death signal. The launcher therefore survives
  a `discobox-server` restart.
- It holds an exclusive lock in the pool's private runtime directory for its
  lifetime and records enough process identity to prevent PID-reuse mistakes.
  A second launcher for the same pool must fail while that lock is held.
- It owns passt's lifecycle. passt terminates when the launcher exits; an
  unexpected passt exit makes the VM unhealthy and terminates the launcher
  rather than leaving a VM with silently broken networking.
- `InspectVM` re-adopts an existing launcher by validating its locked runtime
  state and control sockets. If the launcher died, normal desired-state
  reconciliation starts a replacement with the existing data disk.
- Server shutdown does not delete or stop pool runtimes. Pool deletion, repair,
  an explicit runtime stop, or host shutdown requests guest poweroff and then
  terminates the launcher after a bounded wait.

`discobox-server` is therefore the desired-state supervisor while it is
running, but the OS process tree does not make server liveness a prerequisite
for VM liveness. No separate daemon or systemd unit is required. If both the
server and a launcher are down, reconciliation resumes only when the server
returns; a pool has no useful control-plane service while the server is down
anyway.

### 2. A Nix flake owns the implementation toolchain and runtime closure

Implementation adds a repository Nix flake and committed lock file. The flake
pins nixpkgs and is the canonical way to obtain and build the local-VM
dependencies; implementation and CI do not depend on whichever VMM, firmware,
or image tools happen to be installed by the host distribution.

The flake provides:

- a package containing `discobox-krun` and its runtime closure;
- libkrun from the newest pinned stable 1.x release, rather than the
  incompatible, pre-stable 2.0 development branch;
- the matching `libkrunfw` guest kernel payload;
- passt; and
- a development shell and image-builder package with Docker/BuildKit,
  `e2fsprogs`, and QCOW2 conversion tools.

nixpkgs builds libkrun's optional block and network backends only when enabled.
The flake must therefore use the equivalent of:

```nix
pkgs.libkrun.override {
  withBlk = true;
  withNet = true;
}
```

The selected package is checked for both features during the launcher build or
startup. Updating libkrun, libkrunfw, or passt is one reviewed flake-lock
change.

### 3. passt provides outbound-only IP networking

Each VM gets one conventional virtio-net interface connected through
`krun_add_net_unixstream` to a per-VM passt Unix socket. The launcher and passt
run as the same unprivileged user as `discobox-server`. No network interface or
network namespace is created on the host.

Inbound TCP and UDP forwarding is explicitly disabled, even where that is
passt's default. Host loopback and gateway mappings are disabled as well. The
guest receives DHCP/DNS configuration and may initiate outbound TCP, UDP, and
ICMP traffic, but no guest listener is reachable from the host or LAN through
its IP network.

Using virtio-net preserves normal guest packet networking. Docker bridges,
container network namespaces, embedded DNS, nftables, and the internal network
that forces sandboxes through `discobox-pool-proxy` continue to operate inside
the VM.

All Discobox host/guest control traffic uses VSOCK, never this IP path.
Internal guest and sandbox-container listeners may still exist, but passt does
not expose them.

### 4. HTTP crosses the VM boundary over VSOCK in both directions

VSOCK is a transport for the existing HTTP protocols, not a new RPC protocol.
HTTP routers, request bodies, streaming behavior, and signed authentication
remain unchanged.

libkrun implements host integration for virtio-vsock by translating configured
guest VSOCK ports to host Unix sockets. The host does not bind a native
`AF_VSOCK` CID. `discobox-krun` only declares the port/path mappings while
configuring the VM; libkrun's virtio-vsock backend owns the byte-for-byte data
path. There is no HTTP-aware proxy in the launcher. The mapping is:

- For pool-agent-initiated traffic, the guest dials host CID 2 and a fixed
  control-plane VSOCK port. libkrun connects that stream to the control
  plane's Unix listener.
- For control-plane-initiated traffic, libkrun creates a private per-VM Unix
  listener and turns each accepted host connection into a connection to the
  pool-agent's guest VSOCK listener. `AcquirePoolAgentClient` dials that Unix
  socket.
- A second private per-VM Unix listener maps to a guest VSOCK-to-Docker bridge
  so `AcquireDockerClient` can reach `/var/run/docker.sock` without SSH or a
  TCP Docker listener.
- A guest lifecycle endpoint requests an orderly system shutdown before the
  launcher is terminated. The driver uses a bounded wait and force-terminates
  the launcher only after graceful shutdown fails.

Per-VM runtime directories are accessible only to the server user. Unix socket
permissions and the existing signed pool authentication protect the mappings;
there is no host-global guest CID to allocate or collide.

The pool-agent and control plane accept injected `net.Listener`/HTTP transport
implementations so Docker and cloud providers retain their existing
transports. The local VM starts no pool-agent TCP listener and configures no
passt port forward.

### 5. The guest has one immutable and two writable disks

The versioned guest root artifact is a QCOW2 disk image containing an ext4
filesystem produced from a Dockerfile rootfs. The image build exports the
Dockerfile result, populates the filesystem, and converts it to QCOW2 without
installing a QEMU VMM in the runtime closure. libkrun attaches it with an
explicit QCOW2 format and a read-only flag. The compatible guest kernel and
init payload come from the flake-pinned `libkrunfw`.

Every pool also has two independent writable, partitionless ext4 disk images:

| Disk | Guest mount | Lifecycle |
| --- | --- | --- |
| Root QCOW2 | `/` | Shared immutable artifact; replace on image upgrade |
| Data | `/var/lib/discobox` | Per-pool durable state; preserve across repair |
| Cache | `/var/lib/discobox/cache` | Per-pool disposable cache |

The writable disks are sparse raw files initially. The cache filesystem is a
nested mount over the `cache` directory on the data filesystem.

The data filesystem contains `docker`, `projects`, and `proxy`. After mounting
it, the guest bind-mounts `/var/lib/discobox/docker` onto `/var/lib/docker`.
Docker therefore keeps its conventional path without daemon-specific
`data-root` configuration. Storage mounts complete before Docker starts:

1. mount data at `/var/lib/discobox`;
2. prepare `docker`, `projects`, `proxy`, and `cache`;
3. mount cache at `/var/lib/discobox/cache`;
4. bind `/var/lib/discobox/docker` at `/var/lib/docker`; and
5. start Docker and the guest VSOCK services.

Writable operating-system state that is not Discobox data uses tmpfs. The
read-only root is never given a writable overlay.

### 6. The pool cache root changes globally

Separating cache from durable data is a pool-agent storage contract, not a VM
special case. Every provider uses:

```text
/var/lib/discobox/cache/projects/{project}/pools/{pool}/cache
```

for the shared pool cache. Durable paths do not move:

```text
/var/lib/discobox/projects/{project}/pools/{pool}/sandboxes/{sandbox}/...
/var/lib/discobox/proxy/...
```

On providers without a separate cache filesystem,
`/var/lib/discobox/cache` is an ordinary directory. VM and Kubernetes
providers may mount independently disposable storage there.

Existing caches under
`/var/lib/discobox/projects/{project}/pools/{pool}/cache` are not migrated.
They are disposable and are reclaimed as legacy cache directories. Sandbox
data, sources, config, tombstones, proxy keys, and audit data stay at their
existing durable paths.

Pool cleanup enumerates data, cache, and proxy roots independently. Durable
data retains its tombstone window; a cache or proxy subtree whose corresponding
retained data subtree is gone can be removed immediately.

## Rejected

- **QEMU as the runtime VMM.** QEMU can satisfy the block, VSOCK, KVM, and
  passt requirements, but its generic system emulator and device surface are
  unnecessary for a fixed microVM workload. libkrun exposes the required
  devices through a small stable API and is designed to make a VM behave like
  a supervised process. QCOW2 image conversion may still use `qemu-img` in the
  build-only Nix closure; no QEMU system emulator is installed or run.
- **libkrun TSI networking.** TSI is the closest match to a pure user-process
  network model: it needs neither a guest NIC nor a separate passt process.
  Its kernel-wide userspace socket interception also bypasses Docker's packet
  path and therefore the internal bridge that makes the pool proxy a
  sandbox's only route off-box. Revisit only if libkrun can scope TSI away
  from sandbox network namespaces, or Discobox no longer relies on Docker
  network topology for proxy enforcement.
- **Cloud Hypervisor.** It is easy to install through a Nix flake and supports
  the required block and VSOCK devices. Installation is not the problem. Its
  documented network path is TAP-based, and adapting another networking edge
  removes the simplicity advantage it would otherwise have.
- **Firecracker.** Its small device model fits the workload, but its supported
  network model requires a TAP device. That directly violates the
  process-like host networking requirement.
- **A TAP/veth bridge with host NAT.** This is conventional and fast but
  creates privileged, host-global network state whose cleanup and collision
  handling become part of the driver.
- **Loopback TCP listeners or ephemeral host port forwards.** Loopback reduces
  exposure but still creates listening IP ports, port allocation races, and a
  second transport alongside VSOCK.
- **Run the pool-agent directly as a guest system service.** This would remove
  the need for Docker-over-VSOCK during bootstrap, but it would fork
  `dockerworker.Engine`: image selection, boot metadata injection, drift
  detection, and pool-agent replacement would become local-VM-specific.
- **Move durable state to `/var/lib/discobox/data`.** Mounting the data disk at
  `/var/lib/discobox` preserves the global pool-agent paths. A nested cache
  mount gives the desired independent lifecycle without migrating sandbox or
  proxy state.
- **Configure Docker with a nonstandard data root.** A bind mount from
  `/var/lib/discobox/docker` keeps all durable bytes on the data disk while
  preserving Docker's conventional `/var/lib/docker` path.

## Consequences

- The local VM looks like two ordinary unprivileged user processes
  (`discobox-krun` and passt), regular state files, Unix sockets, and
  `/dev/kvm`; it creates no host network devices.
- A server restart temporarily interrupts control-plane streams but does not
  power off local pools. The restarted server rebinds its stable Unix listener,
  re-adopts the launcher, and the pool-agent's normal retry behavior restores
  its outbound streams.
- Unlike a native host `AF_VSOCK` backend, libkrun terminates guest VSOCK
  streams at private Unix sockets. Bidirectional traffic still crosses the VM
  boundary over virtio-vsock, but host listener construction uses Unix
  sockets.
- The guest base image stays generic and contains no pool bootstrap identity.
  The engine still injects project/pool credentials into the pool-agent
  container.
- The server and pool-agent listener construction must become transport
  neutral. Existing TCP, Unix-socket, and named-pipe behavior remains
  available.
- The guest image adds narrowly scoped VSOCK services for Docker access and
  orderly shutdown. Docker itself never listens on TCP.
- Pool storage reporting uses `/var/lib/discobox`, not `/`, because the root
  filesystem is immutable and does not represent available sandbox capacity.
- Replacing a failed VM reuses its data disk and may reuse or recreate its
  cache disk. Deleting the pool deletes both writable disk images only after
  normal pool deletion has established that no sandbox remains assigned.
- The Dockerfile-to-QCOW2 root build and its flake-pinned libkrunfw payload
  must be tested and versioned as one compatible artifact set.
- The stable libkrun 1.x ABI is isolated behind the launcher protocol. A
  future move to libkrun 2.x changes that helper rather than the server's VM
  driver contract.

## Deferred

- **AArch64 local hosts.** libkrun and libkrunfw support AArch64, but the first
  provider targets x86-64. Revisit when an AArch64 local provider is required.
- **Live disk growth and snapshots.** Initial disk sizes are provider
  configuration. Add online growth or snapshots when fixed-size sparse images
  become an observed operational limitation.
- **Guest inbound IP services.** There is no port-forwarding escape hatch.
  Revisit only if a product requirement cannot be served through the existing
  pool-agent proxy and VSOCK paths.

## References

- [libkrun](https://github.com/libkrun/libkrun)
- [libkrun stable 1.19 API](https://github.com/libkrun/libkrun/blob/stable-1.19.x/include/libkrun.h)
- [libkrun firmware](https://github.com/libkrun/libkrunfw)
- [passt manual](https://passt.top/builds/latest/web/passt.1.html)
- [nixpkgs libkrun package](https://github.com/NixOS/nixpkgs/blob/master/pkgs/by-name/li/libkrun/package.nix)
