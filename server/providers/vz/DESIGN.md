# macOS vz Provider Design

This package implements ADR 0062. It is a `dockerworker.Driver`: the shared
engine still owns the pool-agent container and every Docker mechanic, while this
package owns one Virtualization.framework VM per pool and the connection leases
into it.

## The invariant

A Mac needs a codesigned `discobox-server` binary and network reachability to
the registry. Nothing else — in particular, no Docker daemon on the host, and no
VM image, hypervisor library, or launcher to install.

Everything is ordered off that:

1. `guestimage` pulls the guest boot artifacts by digest and caches them.
2. The VM boots and its Docker daemon comes up.
3. The engine's development image build-mode builds the pool, sandbox-base, and
   harness images on that daemon's BuildKit, from the local checkout.
4. The engine starts the pool-agent container from the image it just built.

The registry is load-bearing only for the first boot on a machine, and only
until a locally built guest exists.

## Process boundary

There is none. Virtualization.framework is part of macOS and is used in
process through `Code-Hex/vz`, so unlike libkrun there is no launcher process,
no runtime lock, and no recorded process identity.

`internal/vzvm` is the whole cgo/darwin surface, isolated exactly as
`wslc/internal/wslcsession` is, so the driver, its configuration, and its tests
compile and run on every platform. Off darwin every entry point returns
`ErrUnsupported`; the provider is registered from `platform_darwin.go` only.

A VM is an in-process object, so **the VM dies with the server** — a property,
not a policy. There is nothing to re-adopt after a restart. `StopVM` and
`DeleteVM` differ only in whether the disks survive: `StopVM` is what repair
uses, `DeleteVM` is reserved for an authorized pool deletion.

Codesigning is part of the build, not of packaging: creating a VM requires
`com.apple.security.virtualization`, and that entitlement exists only on a
signed binary. `task sign` re-signs after every build and is wired into
`build:server`, `build:cli`, `release:binary`, and the watchnbuild dev loop.
`disco` needs the entitlement as much as `discobox-server` does: it runs the
server in-process, so it is the process that creates the VM (ADR 0066 §5).
`go run` cannot start a server that runs pools.

## Transport boundary

Every byte of Discobox control traffic is VSOCK, in both directions, and macOS
opens no TCP listener:

| Port | Direction | Purpose |
| --- | --- | --- |
| 3001 | guest → host | control plane; the driver accepts on a host-side listener and pushes into `carrierhub` |
| 3002 | host → guest | pool-agent API |
| 3003 | host → guest | orderly shutdown, before a hard stop |
| 3004 | host → guest | the guest's Docker socket |

The numbering matches the libkrun guest, so one `discobox-vsock-guest` serves
both backends.

Connections deliberately do not implement `CloseWrite`. The framework's
connection cannot have its write side shut down independently through this
binding, and a no-op `CloseWrite` would be worse than none: a caller that finds
the method believes the guest saw EOF. Callers that probe for it take their
full-close fallback, which is correct. Nothing the engine does over this client
— container lifecycle, BuildKit's grpc session, TTY console attach — needs a
half-close.

The IP network exists only for the guest's own outbound traffic. It is the
framework's NAT attachment, which also serves DHCP and DNS; the guest takes its
address from the lease. Bridged networking is not used: it needs the
`com.apple.vm.networking` entitlement, which Apple grants by request, and it
would put pool guests on the user's LAN.

## Clock

The guest steps its clock to the host's every 30s, reading
`/sys/class/rtc/rtc0/since_epoch` — Virtualization.framework's PL031 RTC is the
host's clock, live, so this needs no NTP server and no network.

It is not optional bookkeeping. Linux reads the RTC once at boot, and nothing
tells the guest that the Mac suspended, so a laptop that sleeps wakes a guest
believing no time passed. Every credential in the system is time-bounded: the
agent's assertions carry a five-minute lifetime, and the control plane's scoped
tokens are parsed with `NewParserForValidNow`. A guest hours behind the host
therefore mints tokens the control plane reads as long expired *and* rejects
control-plane tokens as not yet valid — both surfacing as bare 401s, in both
directions at once.

Two mechanics are load-bearing. `hwclock --hctosys` cannot do this: it waits for
an RTC update interrupt that PL031 never raises, times out, and exits without
setting anything. And the step is unconditional — NTP daemons refuse a large
offset without operator intervention, which is precisely backwards here, since
the offset is large exactly because the Mac slept.

## Guest artifact boundary

Three artifacts, resolved by `server/providers/guestimage`: an uncompressed
kernel (`vmlinux`), an initrd (`initrd.img`), and a read-only raw ext4 root
(`root.ext4`). Disks are attached in a fixed order the guest depends on — root,
data, cache become `/dev/vda`, `/dev/vdb`, `/dev/vdc`.

Sizing comes from the host, not from constants: every vCPU, half the memory
(`vzvm.DefaultHostResources`, clamped to the range Virtualization.framework
reports), and a 100 GiB data disk. None of it is a reservation — vCPUs are
shared with macOS by the scheduler, the guest has a memory balloon, and both
disks are sparse, costing only what the guest writes.

Disk sizes are therefore ceilings a pool can be given more of. `ensureDisks`
grows an existing image when the configured size is raised and never shrinks
one, and the guest runs `resize2fs` on each mount so the filesystem follows.

The root is shared read-only by every pool on the host. Each pool owns
`data.raw` and `cache.raw`, created sparse and formatted by the guest on first
boot. Only raw images exist here: Virtualization.framework has no QCOW2 path.

`image/Dockerfile` produces all three in a final `FROM scratch` stage.
Filesystem assembly runs inside the build, because `mkfs.ext4 -d` needs no loop
device and no privileges — which is what lets the same Dockerfile be built by
CI, by a Linux developer, and by BuildKit inside a running pool VM.

The guest carries only what boots Docker: `dockerd`, the two
`discobox-vsock-guest` services, a storage unit that mounts the data and cache
disks and bind-mounts `/var/lib/docker` and `/var/lib/containerd` onto them, and
`systemd-networkd`/`systemd-resolved` for the DHCP lease. Anything a sandbox
needs belongs in a container on that daemon, never here. That scope rule is what
keeps the image small enough to ship on first boot and stable enough to version
on its own line.

Its image assets duplicate libkrun's rather than sharing them. That is
deliberate: libkrun's guest is slated for rework, and coupling to it first would
make that rework harder. The *pipeline* — `guestimage` and
`dockerworker.BuildArtifacts` — is shared from the start; the image content is
not (ADR 0062 §5).

## Release boundary

The guest image is built and released on its own line
(`.github/workflows/vm-image.yml`, tags `vm/v*`), not with the discobox release.
`DefaultGuestImage` pins what a server build boots. Its inputs change when
Debian, the kernel, or Docker changes rather than when Discobox does, so tying
the two would make a guest fix require a product release and a product release
imply a new guest.

It publishes as `discobox-vm`. The name carries no backend because the artifacts
are not vz's to own — libkrun is expected to boot the same set once ADR 0062 §9
lands — and it is not called a pool image because that already means the
pool-agent container (`dockerworker.DefaultPoolImage`), which this provider
exposes as `workerImage` beside `guestImage`.

The compatibility surface between the two lines is narrow by construction — the
VSOCK port map, the storage layout, and `discobox-vsock-guest`, which is built
from `pool-agent` sources into the guest image. Keeping it narrow is what makes
independent versioning safe; a change to it is a coordinated release.
