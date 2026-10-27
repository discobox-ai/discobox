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

1. `guestimage` fetches the guest image by digest through the server's image
   store and extracts its boot artifacts. A CLI that launched the server has
   staged it there already (ADR 0113), so on a first run this is local work.
2. The VM boots and its Docker daemon comes up.
3. The sandbox base image reaches that daemon. A release pulls its pinned
   image from the registry; in development, build-mode (`DevelopmentImageSync`)
   builds it, and the agent, pool, platform and harness bundles, on the
   daemon's own BuildKit, from the local checkout; the server saves each bundle
   and serves it to pools (ADR 26-10-16-423 §13).
4. The engine starts the pool's container and makes it a pool's box (ADR
   26-10-24-953 §2; see [providers](../DESIGN.md#pool-bundle)).

For the guest, the registry is load-bearing only for a machine's first fetch of
each pinned digest — made by the CLI before the server starts, when a CLI
launched it — and not at all once a locally built guest exists.

## Process boundary

There is none. Virtualization.framework is part of macOS and is used in
process through `Code-Hex/vz`, so unlike libkrun there is no launcher process,
no runtime lock, and no recorded process identity.

In process also means the framework shares the server's descriptor table.
`VZVirtioSocketConnection` owns its descriptor and closes it when the object is
destroyed, so the binding duplicates it and never closes the original. Upstream
`Code-Hex/vz` closes it, which lets the framework's own close land on whatever
reused the number — a connection the Unix listener just accepted, ending
`Serve` with `accept unix …: bad file descriptor`. The binding is therefore the
`discobox-ai/vz` fork, pinned by a `replace` in `server/go.mod`, and
`task test:vz-stress` is the reproduction: a Unix listener probed on fresh
connections while a real guest churns VSOCK.

`internal/vzvm` is the whole cgo/darwin surface, isolated exactly as
`wslc/internal/wslcsession` is, so the driver, its configuration, and its tests
compile and run on every platform. The bindings are built `darwin && cgo` —
`Code-Hex/vz` is an entirely cgo package, so a darwin build with cgo off has no
framework to call — and the stub covers `!darwin || !cgo`. Every stub entry
point that can fail returns `ErrUnsupported` wrapped with which build it is
(`Running` reports false, `Close` succeeds): off macOS the framework does not
exist, on macOS the binary was built `CGO_ENABLED=0`. That second build still
registers the provider, since `platform_darwin.go` is selected by GOOS alone,
so it fails at `NewDriver`'s `Supported` check — on first use, with the fix
named, rather than at init or silently.

A VM is an in-process object, so **the VM dies with the server** — a property,
not a policy. There is nothing to re-adopt after a restart. `StopVM` and
`DeleteVM` differ only in whether the disks survive: `StopVM` is what repair
uses, `DeleteVM` is reserved for an authorized pool deletion.

Codesigning is part of the build, not of packaging: creating a VM requires
`com.apple.security.virtualization`, and that entitlement exists only on a
signed binary. `task sign` re-signs after every build and is wired into
`build:server` and the watchnbuild dev loop. The native macOS CI/release command
builds and signs on a Mac; Dagger validates its source-bound artifacts before
release assembly. `go run` cannot start a server that runs pools.

Native releases sign both executables. Only `discobox-server` receives the
Virtualization entitlement (`vz.entitlements`): the CLI runs the server as a
separate program it downloads (ADR 0099), so the server creates the VM. The signature lives inside the Mach-O and the
release digest is taken after signing, so a staged server — verified against
that digest — arrives signed.

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

## Host filesystem boundary

The guest sees exactly one host directory: `/Users`, exported as a read-only
virtiofs share tagged `discobox-users` and mounted in the guest at `/Users` —
the same absolute path it has on the Mac.

No source delivery reads the share. Every source is delivered by a client into
the pool's cache of Git, and the pool agent serves a sandbox its namespace of
that cache
([ADR 26-10-10-545](../../../docs/adr/26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md)):
nothing clones from, or serves, a path on the Mac. The share, the engine's host
mount of it at `/host/Users`, and the guest's mount point are still made.

Read-only is the whole of the write policy: nothing in a pool or a sandbox may
write to files on the Mac. The host enforces it — the guest's `ro` in
`vm-image/fstab` is a second statement of the same thing, not the one that
counts.

The share stops at the pool. The guest and the pool-agent container see all of
`/Users`; a sandbox sees none of it — nothing is bound into a sandbox. Widening
the share therefore widens what the pool agent can read, not what a sandbox
can.

## Clock

The guest steps its clock to the host's every 30s (`vm-image`'s
`discobox-timesync.timer`), reading `/sys/class/rtc/rtc0/since_epoch` —
Virtualization.framework's PL031 RTC is the host's clock, live, so this needs
no NTP server and no network. The VZ driver also observes macOS's system wake
notification through IOKit and immediately steps each running VM's clock by
executing `date` in its privileged pool-agent container over Docker's VSOCK
connection. That connection has no time-bounded token, so it works even when
the pool-agent API rejects control-plane tokens because of clock skew. The
guest timer remains the fallback when wake notification or Docker exec fails.

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
setting anything. And the step has no upper bound — it skips only drift of 2s
or less, as jitter — because NTP daemons refuse a large offset without operator
intervention, which is precisely backwards here, since the offset is large
exactly because the Mac slept.

## Guest artifact boundary

Three artifacts, resolved by `server/providers/guestimage`: an uncompressed
kernel (`vmlinux`), an initrd (`initrd.img`, declared optional so a guest with
virtio built in still boots), and a read-only raw ext4 root (`root.ext4`).
Disks are attached in a fixed order the guest depends on — root, data, cache
become `/dev/vda`, `/dev/vdb`, `/dev/vdc`.

The kernel command line carries `panic=`, so a panicked guest resets, and the
framework stops a VM whose guest resets. That is what makes `Running` mean
something: without it the kernel halts in place and the VM reports running with
nothing inside. libkrun needs no equivalent — its default command line already
carries `panic=-1`. A guest that is running but broken without panicking is the
engine's to catch, by its Docker daemon not answering a repair.

Sizing comes from the host, not from constants: every vCPU, half the memory
(`vmsize.Host`, the rule every local VM provider shares, clamped by
`vzvm.Clamp` to the range Virtualization.framework reports), a 100 GiB data
disk, and a 50 GiB cache disk. A pool's own size or the provider's
`vcpus`/`memoryMiB` narrows the vCPUs and memory, and a running VM whose size
no longer matches is replaced (`server/providers/DESIGN.md`, Local VM Sizing).
None of it is a reservation — vCPUs are shared with macOS by the scheduler, the
guest has a memory balloon, and both disks are sparse, costing only what the
guest writes.

Disk sizes are therefore ceilings a pool can be given more of. `ensureDisks`
grows an existing image when the configured size is raised and never shrinks
one, and the guest runs `resize2fs` on each mount so the filesystem follows.

The root is shared read-only by every pool on the host. Each pool owns
`data.raw` and `cache.raw`, created sparse and formatted by the guest on first
boot. Only raw images exist here: Virtualization.framework has no QCOW2 path.

Everything lives under the XDG data home (`~/Library/Application
Support/discobox/vz`): `guest/` holds the artifacts extracted from each digest,
`guest/local/` is where a local guest build lands (`guestImageLocalDir`), and
`pools/<poolID>/` holds `data.raw`, `cache.raw`, and `console.log`.
`guestImageDir` bypasses resolution and boots a directory as-is, failing rather
than falling back.

The guest itself is not vz's. `vm-image/` builds one image for every VM
backend, and `vz` boots its `linux/arm64` variant while libkrun boots the
`linux/amd64` one (ADR 0101 §1); `vm-image/DESIGN.md` is where its scope rule,
its assembly, and its compatibility surface are described. Nothing in it names
either backend, and the one difference that could have — how the guest gets an
address — is answered the same way for both, because passt serves DHCP as
Virtualization.framework's NAT attachment does.

What is vz's is which artifacts it asks the resolver for and how it attaches
them, above.

## Progress and pool logs

`EnsureVM` resolves the guest outside the driver lock and reports
`PoolPhaseFetchingVMImage` — once up front, then restated with byte counts while
a pull moves bytes, never held. `PoolLogs` tails `console.log`, the serial
console the VM appends to across every boot, so a guest that never reached
Docker still leaves a record. The rules shared with other drivers are in
[`../DESIGN.md`](../DESIGN.md).

## Guest build loop

A Mac has no Docker daemon, so the only builder that can produce a guest image
is the one inside a pool VM — a VM booted from the guest image being replaced.
The engine's `BuildGuestImage` closes that loop (ADR 0062 §7) from the driver's
`GuestImageBuildSpec` — `vm-image/Dockerfile`, `linux/<host GOARCH>`, and the
resolver's local directory: the running guest builds its successor on its own
BuildKit, the artifacts come back over the same Docker transport as a `local`
export, and they land in `GuestImageLocalDir`, which the resolver already
prefers over the published image. An instance configured with `guestImageDir`
has no local directory to adopt into, so it reports
`ErrGuestImageBuildUnsupported`.

Three things make it work rather than merely run:

- **It needs the pool's Docker, not its agent.** The build is reached through
  the driver, like the console and the host log, so it answers on a pool whose
  agent never started — which is the pool a broken guest image produces, and
  therefore the only pool anybody would be trying to rebuild a guest from.
- **The artifacts are staged and swapped.** A pool may be booting from that
  directory at this moment; a build writing into it directly would replace a
  kernel underneath a VM reading it. A build that exports nothing is refused
  rather than published, because an incomplete local build is one the resolver
  skips in silence.
- **The resolver's memo is dropped on adoption.** It resolves once per process,
  which is right for a value nothing can change under a running server — except
  this. Without it the server keeps booting the guest it resolved first for as
  long as it runs, and the build appears to have done nothing.

A running VM keeps the artifacts it started with, so adopting a new guest means
restarting the pool's host. `discobox admin pool build-guest --restart` does it
once the build lands: the engine calls `StopVM`, the disks survive, and the
pool's reconcile boots the new guest. It is opt-in because it stops everything
running on the pool.

## Release boundary

The guest image is built and released on its own line
(`.github/workflows/vm-image.yml`, tags `vm/v*`), not with the discobox release.
`DefaultGuestImage` is `guestimage.DefaultVMImage`, and the pin lives there
rather than here because the image is shared: one publish has to be one edit, or
a backend quietly keeps booting the release before last. Its inputs change when
Debian, the kernel, or Docker changes rather than when Discobox does, so tying
the two would make a guest fix require a product release and a product release
imply a new guest.

It publishes as `discobox-vm`. The name carries no backend because the artifacts
are not vz's to own — libkrun boots the same image on amd64 — and it is not
called a pool image because that means the image a pool's container runs, the
sandbox base image (`dockerworker.DefaultSandboxBaseImage`), which runs on the
guest this boots.

The compatibility surface between the two lines is narrow by construction — the
VSOCK port map, the storage layout, the `discobox-users` share tag and where the
guest mounts it, and `discobox-vsock-guest`, which is built from `pool-agent`
sources into the guest image. Keeping it narrow is what makes independent
versioning safe; a change to it is a coordinated release.

In development neither line is what runs: `task build:vm-guest` (or
`discobox admin pool build-guest`, on a Mac with no daemon) builds the guest
from the checkout and the local build wins over both. That is the intended way
to work on `vm-image/`, and to run a guest-side change without cutting a release
for it.

The share is the one place that ordering is not symmetric. A guest whose host
attaches nothing comes up with an empty `/Users` (`nofail`), but a server that
declares the host mount against a guest with no `/Users` at all cannot start a
pool container: Docker will not bind a source the daemon does not have. So a
change to a share ships guest first — a `vm/v*` tag and a re-pinned
`guestimage.DefaultVMImage` — and the provider's host mount lands with or after
it.
