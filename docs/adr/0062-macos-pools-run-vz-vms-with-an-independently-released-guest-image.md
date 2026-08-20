# 0062. macOS pools run Virtualization.framework VMs, and the VM guest image is an independently released artifact

Status: Accepted

Date: 2026-08-19

## Context

Discobox runs a pool on the host Docker daemon, on a DigitalOcean droplet, on a
libkrun microVM (Linux/KVM, ADR 0013), or in a WSL Containers VM (Windows,
`wslc`). macOS has no backend at all.

Most of what a macOS backend needs already exists, because Windows needed it
first:

- `dockerworker.Driver` is a four-method VM lifecycle plus two connection
  leases, and the leases are expressed as a dialer, so a new hypervisor adds no
  Docker knowledge above the driver.
- Development images already build on the *destination* daemon. The image
  watcher's build-mode is the default off Linux and stamps a manifest instead of
  building; `dockerworker/image_build.go` then drives the destination daemon's
  embedded BuildKit over `DialHijack("/grpc")`, streaming the repository as the
  local build context. A host with no Docker daemon is already a supported
  development host.
- `carrierhub.Hub` serves the ordinary control-plane handler over connections
  the guest opened, so no backend needs the server to listen on TCP.
- `pool-agent/endpoint` already carries VSOCK in its URL scheme, in both
  directions.

One gap is not covered, and it is the whole difficulty: every VM backend so far
obtained its guest from somewhere the Mac does not have. `wslc`'s base VM is
already installed and already has Docker. libkrun builds its root image and its
kernel *on the host* with `docker-buildx`
(`server/providers/libkrun/image/build-root-image.sh`,
`server/providers/libkrun/kernel/build-kernel.sh`, and the `docker-client` /
`docker-buildx` / `fakeroot` / `qemu-utils` runtime inputs in `flake.nix`). On
macOS there is neither a preexisting guest nor a Docker daemon to build one
with, and requiring Docker Desktop is precisely the dependency this backend
exists to avoid — a host Docker daemon is also a weaker boundary than the
per-pool VM, so it would be a worse backend even if it were free.

Two further pieces of ADR 0013 are in scope here, because the macOS backend
answers them differently and libkrun should converge: the launcher is a separate
`discobox-krun` binary whose lifetime is deliberately decoupled from the server
(§1), and a Nix flake owns a toolchain that includes Docker for building guest
artifacts (§2).

## Decision

### 1. The macOS backend is a `vz` driver that owns its VM in-process

`server/providers/vz` is a `dockerworker.Driver` built on
Virtualization.framework through `Code-Hex/vz`, `//go:build darwin`, registered
from a new `platform_darwin.go` as `wslc` is from `platform_windows.go`.
`dockerworker.Engine` continues to own the pool-agent container and every Docker
mechanic.

The framework is part of the operating system. Nothing is shipped alongside the
server, and nothing is embedded in it.

A VZ virtual machine is an in-process object, so the wslc lifetime rule is not a
choice here but a property: **the VM dies with the server.** There is no
launcher, no runtime lock, no PID and process-start-time identity, and no
re-adoption across a server restart. Per-pool `data.raw` and `cache.raw` persist;
the VM does not.

Transport is VSOCK in both directions, terminated by the driver itself:

- `AcquireDockerClient` connects the guest's Docker VSOCK port and hands the
  resulting `net.Conn` to `NewDockerClientForDialer`.
- `AcquirePoolAgentClient` connects the pool-agent's VSOCK port.
- Guest-initiated control-plane traffic is accepted by a host-side
  `VirtioSocketListener` and pushed into `carrierhub.Hub`.
- `StopVM` requests orderly poweroff through the existing
  `discobox-vsock-guest lifecycle` service and preserves the disks; `DeleteVM`
  removes them.

macOS opens no TCP listener for any of this.

Creating a VM requires the `com.apple.security.virtualization` entitlement, so
codesigning the server binary is part of the darwin build rather than a
packaging step: an unsigned binary cannot start a pool.

The initial target is Apple Silicon.

### 2. The macOS host never needs Docker, and the bootstrap order is fixed

The invariant is: **a codesigned server binary and network reachability to the
registry. Nothing else, ever, including for guest artifacts.**

Everything else is strictly ordered off that:

1. Guest artifacts are resolved from the registry by digest and cached.
2. The VM boots and its Docker daemon comes up.
3. The existing build-mode path builds the pool, sandbox-base, and harness
   images on that daemon's BuildKit from the local checkout.
4. The engine starts the pool-agent container from the image it just built.

The registry is load-bearing only for the first boot on a given machine, and
only until a locally built guest exists (§7).

### 3. The guest image is an independently versioned, released artifact

The guest image gets its own CI build and its own release line, published to the
registry as an OCI image whose layers carry the boot artifacts. It is **not** cut
with the discobox release, which today stages and publishes `worker-agent` and
`sandbox-agent` under a single tag from `.dagger/modules/release`.

The reason is that its inputs move on a different clock. It is a distribution
userland that boots `dockerd` and two VSOCK helpers; it changes when the
distribution, the kernel, or Docker changes, not when discobox does. Publishing
it per discobox tag would push hundreds of megabytes per release, and — the part
that actually matters — would make a guest fix require a product release and a
product release imply a new guest.

The server pins a **digest**, not a floating tag, so a given server build always
boots a known guest, a guest release does not force a server release, and a
server release does not imply a new guest. Rebuilds happen when the guest's own
inputs change and on a periodic cadence for security updates.

### 4. The guest is Docker's host and nothing else

If something is not required to bring `dockerd` and the VSOCK helpers up, it does
not belong in the guest image: no sandbox tooling, no language toolchains, no
agents. Everything the product does happens in containers on that daemon.

This is a scope rule, not an aspiration — it is what keeps the image small
enough to ship from a registry on first boot, static enough to version on its own
line, and small enough to audit.

### 5. Guest artifact resolution is a shared `server/providers/guestimage` package

A peer of `dockerworker`, usable by any VM driver: resolve a reference by digest,
pull with `go-containerregistry` (already a direct dependency of the server
module — no daemon and no `crane` binary), verify, cache per digest under the
platform state directory, and accept a local override directory.

It is provider-neutral from the first commit specifically so that libkrun adopts
it rather than growing a second copy.

### 6. The guest image's Dockerfile produces the whole artifact set

The image build ends in a `FROM scratch` stage carrying the kernel, the initrd,
and the root filesystem. Filesystem assembly happens *inside* the build —
`mkfs.ext4 -d` needs no loop device and no privileges — rather than in a host
script.

This is what lets one Dockerfile be built identically by CI, by a Linux
developer, and by BuildKit inside a running pool VM. It also removes the host
`fakeroot` / `e2fsprogs` / `qemu-img` steps from libkrun's build when it adopts
this.

### 7. A locally modified guest image is built in the VM and exported back

Editing the guest image on macOS uses the pool VM that is already running: build
the guest Dockerfile on that VM's BuildKit over the existing Docker transport,
export the artifacts back over the same session into the host cache, point the
provider at them, and recreate pools.

The self-hosting loop closes: the registry seeds the first VM, and from then on
the VM builds its own successor.

### 8. Kernel and root filesystem come from the distribution, in raw form

The kernel and initrd are the guest distribution's own packaged kernel. VZ
requires an uncompressed kernel image, so the decompression happens at image
build time on Linux and the published artifact is already bootable.

The root filesystem is a read-only **raw** ext4 image: VZ accepts only raw disk
images, so QCOW2 is unavailable. Per-pool `data.raw` and `cache.raw` keep the
independent durable/disposable split libkrun established.

### 9. libkrun converges on this style

When libkrun is reworked it adopts §3, §5, and §6 — its host build scripts and
their Nix Docker/fakeroot/QCOW2 inputs disappear — and its launcher changes
shape:

- The separate `discobox-krun` binary is replaced by a hidden subcommand of the
  server binary, forked from `os.Executable()`. `krun_start_enter` consumes its
  calling process, which is why a dedicated process is still required; it is not
  why a dedicated *artifact* is required.
- libkrun is `dlopen`ed lazily in that subcommand rather than linked, so the
  server binary carries no link-time native dependency and a user who never
  enables the provider never needs the libraries present — the same failure
  ergonomics as `relay.ErrNotBuilt`.
- The VM dies with the server, via `PR_SET_PDEATHSIG` plus a pipe watchdog for
  the fork/prctl race. The runtime lock, the recorded process identity, and
  re-adoption are removed.

On landing, this supersedes ADR 0013 §1's decoupled launcher lifetime and the
part of §2 that puts guest-artifact building on the host.

## Consequences

- Developing discobox on macOS requires a codesigned server binary and a
  network. There is no Docker Desktop, no Colima, no Nix, and no VM to install
  by hand.
- Three backends — `vz`, `wslc`, and libkrun after §9 — share one lifetime rule:
  the VM dies with the server, the disks do not. `server/providers/DESIGN.md`
  states it once instead of each driver arguing it.
- A server restart will kill running pools on Linux, which it does not today.
  The cost is restart latency rather than data: sandbox state lives in named
  Docker volumes on `data.raw`, which survives.
- The server binary must be re-signed on every rebuild, so `go run` stops working
  for the server on darwin and watchnbuild's build command grows a darwin branch.
  Distribution additionally needs Developer ID signing and notarization.
- The darwin server can only be built on macOS, because `Code-Hex/vz` is cgo and
  Objective-C. CI cannot cross-build it from a Linux runner.
- There are two release lines to operate. The compatibility surface between them
  is narrow by construction — the VSOCK port map, the storage layout, and the
  `discobox-vsock-guest` helper built from `pool-agent` sources — and keeping it
  narrow is what makes independent versioning safe. It is not zero, and a change
  to it is a coordinated release.
- Harness and sandbox images must exist for the guest's architecture.
- `Code-Hex/vz`'s `VirtioSocketConnection` exposes deadlines but does not forward
  `CloseWrite`. Docker exec and attach depend on half-close — `wslc` has a
  dedicated e2e test for exactly this failure, which is silent until it
  deadlocks a request — so the driver must supply it, by an upstream addition or
  a wrapper.

## Alternatives

**Require Docker Desktop or Colima on macOS.** Rejected: it is the dependency
this backend exists to remove, and it would also be the weakest available
boundary, since pools would once again share a host daemon.

**libkrun's EFI/Hypervisor.framework variant (krunkit) on macOS.** Rejected, and
worth stating because it is a real option rather than a missing one: it boots
firmware plus a disk image rather than a kernel directly, and it would put a Rust
launcher and Nix-installed native libraries on every developer's Mac — exactly
what §9 removes on Linux. Virtualization.framework ships with the OS.

**Build a purpose-built kernel, as libkrun does.** Rejected: libkrun's kernel
exists because libkrunfw requires patches. VZ is stock virtio, so a distribution
kernel plus its initrd costs no configuration to maintain and no build to pin.

**A squashfs root filesystem.** Rejected: the root is read-only either way, and
raw ext4 avoids depending on squashfs support being present in the initrd.

**Extract and decompress the kernel at runtime on the Mac.** Rejected: it puts a
compression-format scanner in the server for a transformation that is fully
determined at image build time.

**Publish the guest image with the discobox release.** Rejected per §3: it
couples two artifacts whose inputs change at unrelated rates, in the direction
that makes both releases more expensive.

**Embed the guest artifacts in the server binary, as `wslc` embeds its relay.**
Rejected on size: the relay is about a megabyte compressed, while a kernel,
initrd, and root filesystem are two to three orders of magnitude larger, and
`go:embed` would put them in every server binary on every platform, including
those with no VM backend.

**Share libkrun's guest image Dockerfile now.** Rejected as premature: it is
slated for rework, and coupling to it first would make that rework harder. The
*pipeline* (§5, §6) is shared from the start; the image content is not.

**Keep the Rust launcher and merely `go:embed` it.** Rejected: the separate file
is the smaller half of the objection. The runtime lock, process identity, and
re-adoption protocol are the complexity, and re-exec removes the file and the
protocol together.

## Deferred

- **Rosetta.** `VZLinuxRosettaDirectoryShare` can be registered as a binfmt
  handler in the guest so `linux/amd64` harness images run on Apple Silicon. Not
  enabled initially; the guest image must not foreclose it. Revisit when an
  amd64-only harness image is actually required.
- **One guest image for both `vz` and libkrun.** Revisit once §9 has landed and
  the two images differ only in network mode; converging before then would be
  the coupling §5's alternative rejects.
- **Intel Macs.** Virtualization.framework supports them, and the same pipeline
  would produce an amd64 guest. Revisit on demand rather than carrying a second
  published architecture with no consumer.
