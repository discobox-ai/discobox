# Local libkrun Provider Design

This package implements ADR 0013. It is a `dockerworker.Driver`: the shared
engine still owns the pool-agent container and Docker behavior, while this
package owns one local libkrun VM per pool.

## Process boundary

`launcher/` is a standalone Rust binary linked to libkrun only by the Nix
package. The Go server never loads libkrun into its process. The launcher owns
the VM configuration, the runtime lock, passt, and the libkrun event loop.
Provider initialization does not open `/dev/kvm`; the launcher's validation
operation checks KVM access immediately before a VM is started.

New provider instances default to `libkrun`-named XDG state and runtime
directories. When no directory is configured and the pre-rename `local-vm`
directory exists, the driver keeps using it so existing disks and running
launchers remain adoptable.

The server starts the launcher in an independent process session and reconciles
it from the pool runtime directory. Launcher identity consists of the held
`launcher.lock` plus the PID and Linux process start time in `launcher.json`;
never act on a PID without checking both.

`StopVM` requests shutdown and preserves `data.raw` and `cache.raw` for repair.
`DeleteVM` is reserved for an authorized pool deletion and removes both disk
images. `Driver.Close` deliberately leaves launchers running for re-adoption by
the next server process.

## Transport boundary

The guest uses virtio-vsock. libkrun terminates configured VSOCK ports at Unix
sockets:

- guest-to-host control-plane traffic reaches the server's stable Unix socket;
- host-to-guest pool-agent traffic uses a private per-pool Unix socket;
- host-to-guest Docker API traffic uses another private per-pool Unix socket;
- host-to-guest lifecycle traffic requests orderly shutdown.

passt is outbound-only and supplies the guest's conventional virtio-net
interface. Each isolated passt process uses the same fixed private IPv4
address, gateway, and DNS-forward address. The launcher resolves the forwarding
target from the host's first IPv4 nameserver, while a guest oneshot service
configures the interface and route without DHCP or a resolver daemon. Do not
enable TSI or add a host TAP/TUN/veth device.

## Guest artifact boundary

The guest artifact set is a trusted external ELF kernel plus a read-only root
QCOW2. Data and cache are independent raw ext4 images. The launcher loads the
kernel with `krun_set_kernel` and always attaches the disks in root/data/cache
order as `/dev/vda`, `/dev/vdb`, and `/dev/vdc`.

`image/Dockerfile` defines the guest filesystem. The flake-provided
`discobox-build-root-image` exports that rootfs, populates an ext4 image without
loop mounts, and converts it to QCOW2. The guest mounts data at
`/var/lib/discobox`, cache at `/var/lib/discobox/cache`, and bind-mounts
`/var/lib/discobox/docker` at `/var/lib/docker` and `/var/lib/discobox/containerd`
at `/var/lib/containerd` before Docker starts. containerd's own state must live
on the data disk too, not just dockerd's: with the containerd snapshotter,
`/var/lib/containerd` holds the actual image/layer content and every
container's filesystem snapshot, not merely reconstructable bookkeeping — losing
it is not recoverable the way losing `/var/lib/docker` alone once was.

`kernel/Dockerfile` builds the pinned, libkrun-patched Linux kernel with the
network namespaces, bridge/veth, overlayfs, nftables/NAT, x_tables
compatibility, and common Docker network link drivers built in. The
flake-provided `discobox-build-kernel` only invokes this Docker build; Nix does
not compile Linux.

The guest image carries four local-VM-specific services:

- `discobox-network` configures the fixed private passt address and route;
- `discobox-storage` mounts data and cache and prepares Docker's bind mounts;
- `discobox-vsock-guest docker-proxy` byte-splices VSOCK port 3004 to
  `/var/run/docker.sock`;
- `discobox-vsock-guest lifecycle` accepts an HTTP shutdown request on VSOCK
  port 3003 and invokes systemd poweroff.

The pool-agent container listens directly on VSOCK port 3002. Its outbound
control-plane HTTP dials host CID 2 on port 3001. None of these services opens
an IP listener.
