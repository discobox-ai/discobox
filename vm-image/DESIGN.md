# Pool VM Guest Image Design

One guest for every VM backend, and the host runtime one backend cannot take
from it.

This directory implements ADR 0062 §3, §4, §6, ADR 0101 and ADR 0148 §5. It is not a Go
module and is imported by nothing — its one Go binary, `discobox-vsock-guest`,
is compiled from `pool-agent` sources inside the build. What it produces are
boot artifacts, published to a registry on their own release lines and pulled
by `server/providers/guestimage`.

## What is here

| Path | Publishes | Release line | Booted by |
| --- | --- | --- | --- |
| `Dockerfile` | `vmlinux` (arm64 only), `initrd.img`, `root.ext4` | `vm/v*` → `discobox-vm`, `linux/arm64` + `linux/amd64` | `vz` (arm64); libkrun through `discobox-vm-krun` |
| `libkrun/Dockerfile` | `vmlinux`, `kernel.config`, `libkrun.so.1`, `passt` | `libkrun-runtime/v*` → `discobox-libkrun-runtime`, `linux/amd64` | nothing directly; packaged below |
| `libkrun/package/Dockerfile` | `root.ext4` + the four runtime files | `vm-krun/v*` → `discobox-vm-krun`, `linux/amd64` | `libkrun` |

Three builds, three workflows (`.github/workflows/vm-image.yml`,
`libkrun-runtime.yml`, `vm-krun.yml`). The split is not bookkeeping: the runtime
build compiles Linux and libkrun, so a guest change must not trigger it, and
`vm-image.yml` excludes `vm-image/libkrun/**` from its paths for exactly that
reason. The package build compiles nothing — it copies from a pinned
`discobox-vm` digest and a pinned runtime digest (its `GUEST_IMAGE` and
`RUNTIME_IMAGE` build args) — so a guest release reaches libkrun as one
packaging publish and one pin edit, not a kernel compile.

Publishing is tag-driven only: `vm:publish`, `vm:publish-libkrun-runtime` and
`vm:publish-krun` push to `ghcr.io` and report the digest to pin. The scheduled
runs — weekly for the guest, monthly for the runtime — build and verify but
never publish, so a security rebuild is still a release someone deliberately
pins.

Every pin is a digest, so the four lines — product, guest, runtime, libkrun
image — move independently in both directions:

- `guestimage.DefaultVMImage` pins the guest `vz` boots;
- `libkrun/package/Dockerfile` pins the guest and runtime it packages;
- libkrun's `DefaultImage` (`server/providers/libkrun/provider.go`) pins
  `discobox-vm-krun`.

`REVIEW.md` beside this file carries the pitfalls — the rules whose violation
is a guest that boots and then fails somewhere else.

## The scope rule

If something is not required to bring `dockerd` and the two VSOCK helpers up, it
does not belong here: no sandbox tooling, no language toolchains, no agents.
Everything the product does happens in containers on the daemon this image
boots.

That is what keeps the image small enough to ship from a registry on a machine's
first pool start, static enough to version on its own line, and small enough to
audit. The same rule applies to hardware: the guest's entire surface is virtio,
so the build deletes the driver classes Debian ships for the rest of the world
and builds the initrd from a list rather than `MODULES=most`.

## Why one image

Two backends boot this, and nothing in it says so. The guest never learns which
hypervisor it woke up on, and the two places that could have told it are
deliberately answered the same way for both:

- **Networking is DHCP.** passt and Virtualization.framework's NAT attachment
  both serve it. The addressing a libkrun guest gets is still fixed and still
  the launcher's to choose — it is passed to passt as `--address`, `--gateway`,
  `--dns` — but the guest asks for a lease rather than restating it
  (ADR 0101 §2). The one thing the two hosts genuinely differ on is what the
  NIC is called: virtio-pci is `enp0s*`, libkrun's virtio-mmio NIC has no
  topology to name itself from and stays `eth0`, so the unit matches both.
  `vm:verify` asserts both, because an image that got either wrong would boot on
  exactly one backend and fail on the other as a guest that comes up and answers
  nothing.
- **`network-online.target` has to mean something.** `systemd-networkd-wait-online`
  is enabled, not masked, and that is deliberate: it is the only thing that
  gives that target a meaning under networkd, and `docker.service` orders itself
  after it. Masked, the target is reached vacuously and `dockerd` can start
  before the lease lands, with no resolver and no route, failing its first
  registry pull — on both backends, since both take their address from DHCP.
  `vm:verify` asserts the unit is unmasked and linked into
  `network-online.target.wants`. The drop-in narrows the wait to `--any` with a
  timeout, because `docker0` and the per-sandbox bridges are networkd-managed
  too and only exist once `dockerd` is already running, so waiting for every
  link would deadlock against the service waiting on it.
- **The host share is `nofail`.** `vz` attaches `/Users` over virtiofs; libkrun
  attaches nothing. A guest whose host shares nothing comes up with an empty
  `/Users` rather than dropping to an emergency shell. It also prints
  `FAILED Failed to mount /Users` on the way, on every libkrun boot: that line
  is expected, and it is the only failed unit this image tolerates — everything
  else that would fail on one backend is masked or conditioned, because the
  serial console is the one diagnostic a guest that never came up leaves behind.

The clock timer is the same shape of tolerance: every 30 seconds it steps the
guest clock from a host-backed RTC, and its service is conditioned on that RTC
existing, so where there is none it never runs.

## Why libkrun's runtime is separate

libkrun boots a kernel directly through libkrunfw's patched entry paths, so no
distribution kernel boots under it. `vz` is stock virtio and takes Debian's,
which is why ADR 0062 §8 declined to build one at all.

`libkrun/` is therefore a separate build with a separate release line, not a
stage of the guest build. The inputs move on unrelated clocks — it changes
when libkrunfw, libkrun, passt or upstream Linux does, the guest when Debian or
Docker does — and folding them together would make every guest rebuild compile
a kernel and every kernel bump republish a userland (ADR 0101 §3).

The kernel, libkrun, and passt are one build because they change together: the
libkrunfw patches are what that libkrun version expects of its kernel
(ADR 0148 §5). What it builds is what ships inside a release, onto hosts this
repository knows nothing about:

- **Debian's toolchain, not Nix's.** Built on the guest's Debian, nothing needs
  a newer glibc than the guest's userland, and nothing names a `/nix/store`
  RUNPATH or interpreter that exists only where it was built.
- **Upstream libkrun, without libkrunfw.** The provider hands libkrun its own
  kernel, and upstream opens libkrunfw lazily and only when it is not given
  one, so its 21 MB copy of a kernel is never shipped.
- **passt static.** It is the one executable here; it brings no loader or
  library with it.

The kernel comes from a checksum-pinned libkrunfw commit and kernel tarball.
`configure-kernel` builds in what libkrunfw's compact nftables-only baseline
leaves out and the guest needs — Docker's `x_tables` / `iptables-nft`
compatibility, `PACKET` for the DHCP client, macvlan, ipvlan, VXLAN and
802.1Q — and fails the build if `olddefconfig` drops any required setting. The
output is an ELF `vmlinux`, which libkrun loads with no initramfs, plus the
`kernel.config` it was built from, so what booted can be read back off the
artifact.

No host pulls the runtime on its own. `libkrun/package/` puts it beside the
guest's `root.ext4` in `discobox-vm-krun`, the one image libkrun resolves, so a
host that can use any of the five files has all of them under one pin.

The guest build still installs `linux-image-<arch>` on both architectures, so it
has no per-backend branch in it. Assembly publishes `vmlinux` only where it can
produce an uncompressed image a hypervisor will load, which today is arm64; the
package takes `root.ext4` alone from the amd64 guest.

## Assembly

`assemble-guest-image` runs *inside* the build. `mkfs.ext4 -d` populates an
image file with no loop device and no privileges, which is what lets the same
Dockerfile be built by CI, by a Linux developer, and by BuildKit inside a
running pool VM (ADR 0062 §6, §7). There is no host-side `fakeroot`, `qemu-img`,
or loop mount to be missing on a machine with no Docker daemon of its own.

It lifts the kernel and initrd out of `/boot` into artifacts of their own — on
arm64, detecting whether Debian's kernel is already a raw `Image` or which
compression to undo — so the root filesystem never carries them twice.

It also supplies the three files a container runtime provides and a VM does not
— `/etc/hostname`, `/etc/hosts`, and a `/etc/resolv.conf` that is not the
builder's — and sizes the root filesystem to its contents, because every byte of
slack is a byte pulled over the network on a machine's first pool start.

## Working on it

```
task build:vm-guest     # build and stage where this host's provider finds it
task build:vm-krun      # libkrun's runtime plus the guest, Linux only
```

Both stage into the provider's local build directory, which resolution prefers
over the published image when it is complete. On macOS `build:vm-guest` builds
`linux/arm64` into `vz`'s directory. On Linux libkrun's local directory
(`libkrun/.images/vm/local`) holds all five files, so `build:vm-krun` builds the
runtime and the amd64 guest from source and swaps both in, and `build:vm-guest`
then replaces only `root.ext4` — refusing when no runtime is staged there yet.
Building is the whole act of adopting a local image; deleting the directory is
the whole act of going back. Resolution is memoized per server process, though,
so a server that has already resolved an image boots the new build only after
it restarts.

A host with no Docker daemon of its own builds the guest on a pool's instead:
`discobox admin pool build-guest` builds this Dockerfile on the pool VM's Docker
through the driver's `GuestImageBuildSpec`, exports the artifacts into the same
local directory, and drops the resolver's memo (ADR 0062 §7). `vz` and libkrun
both support it; the runtime is not buildable this way. See
[`server/providers/vz/DESIGN.md`](../server/providers/vz/DESIGN.md) and
[`server/providers/libkrun/DESIGN.md`](../server/providers/libkrun/DESIGN.md).

CI runs `task vm:build` / `vm:verify` into `build/vm`,
`vm:build-libkrun-runtime` / `vm:verify-libkrun-runtime` into
`build/libkrun-runtime`, and `vm:build-krun` / `vm:verify-krun` into
`build/vm-krun`. Verification is not optional politeness: a guest that
publishes a kernel the hypervisor cannot load, or a root filesystem that will
not mount, fails as a blank console with nothing to read. `vm:verify` checks
the kernel format per architecture, the root label, the hostname, and the
network properties above. `vm:verify-libkrun-runtime` checks the kernel's ELF
header and required config, that `libkrun.so.1` is a shared object with that
soname and no `NEEDED` on libkrunfw, that `passt` is static, and that nothing
names `/nix/store`; `vm:verify-krun` repeats those on the packaged image and
checks its `root.ext4` label, so a pin to the wrong digest fails there.

## Compatibility surface

The guest is released separately from the server, so what passes between them is
a contract:

- the VSOCK port map — 3001 control plane (guest → host), 3002 pool agent, 3003
  lifecycle, 3004 Docker;
- the storage layout — `/dev/vda` root read-only, `/dev/vdb` data at
  `/var/lib/discobox` (Docker's and containerd's state bind-mounted from it),
  `/dev/vdc` cache at `/var/lib/discobox/cache`. The host creates both writable
  disks empty and sparse; the guest formats them on first boot and grows each
  filesystem to its device;
- the `discobox-users` virtiofs tag and the path it mounts at;
- `discobox-vsock-guest`, built here from `pool-agent` sources.

Keeping it narrow is what makes independent versioning safe. A change to any of
it is a coordinated release: the guest ships first, then the pin moves.
