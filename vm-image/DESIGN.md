# Pool VM Guest Image Design

One guest for every VM backend, and the kernel one backend cannot take from it.

This directory implements ADR 0062 §3, §4, §6 and ADR 0101. It builds no Go and
is imported by nothing: what it produces are boot artifacts, published to a
registry on their own release lines and pulled by `server/providers/guestimage`.

## What is here

| Path | Publishes | Release line | Booted by |
| --- | --- | --- | --- |
| `Dockerfile` | `vmlinux` (arm64 only), `initrd.img`, `root.ext4` | `vm/v*` → `discobox-vm`, `linux/arm64` + `linux/amd64` | `vz` (arm64), `libkrun` (amd64) |
| `kernel/Dockerfile` | `vmlinux`, `kernel.config` | `vm-kernel/v*` → `discobox-vm-kernel`, `linux/amd64` | `libkrun` |

Two builds, two workflows (`.github/workflows/vm-image.yml`, `vm-kernel.yml`).
The split is not bookkeeping: the kernel build compiles Linux, so a guest change
must not trigger it, and `vm-image.yml` excludes `vm-image/kernel/**` from its
paths for exactly that reason.

The server pins digests, so the three lines — product, guest, kernel — move
independently in both directions.

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
  after it. Masked — as it once was, among units a headless guest genuinely does
  not need — the target was reached vacuously and `dockerd` could start before
  the lease landed, with no resolver and no route, failing its first registry
  pull. This matters on both backends now that both take their address from
  DHCP. The drop-in narrows the wait to `--any` with a timeout, because
  `docker0` and the per-sandbox bridges are networkd-managed too and only exist
  once `dockerd` is already running, so waiting for every link would deadlock
  against the service waiting on it.
- **The host share is `nofail`.** `vz` attaches `/Users` over virtiofs; libkrun
  attaches nothing. A guest whose host shares nothing comes up with an empty
  `/Users` rather than dropping to an emergency shell. It also prints
  `FAILED Failed to mount /Users` on the way, on every libkrun boot: that line
  is expected, and it is the only failed unit this image tolerates — everything
  else that would fail on one backend is masked or conditioned, because the
  serial console is the one diagnostic a guest that never came up leaves behind.

The clock unit is the same shape of tolerance: it steps the guest clock from a
host-backed RTC and exits quietly where there is none.

## Why the kernel is not one of them

libkrun boots a kernel directly through libkrunfw's patched entry paths, so no
distribution kernel boots under it. `vz` is stock virtio and takes Debian's,
which is why ADR 0062 §8 declined to build one at all.

`kernel/` is therefore a separate artifact with a separate release line, not a
stage of the guest build. The inputs move on unrelated clocks — this changes
when libkrunfw or upstream Linux does, the guest when Debian or Docker does —
and folding them together would make every guest rebuild compile a kernel and
every kernel bump republish a userland (ADR 0101 §3).

The guest build still installs `linux-image-<arch>` on both architectures, so it
has no per-backend branch in it. Assembly publishes `vmlinux` only where it can
produce an uncompressed image a hypervisor will load, which today is arm64;
libkrun asks the resolver for `root.ext4` alone and never extracts the rest.

## Assembly

`assemble-guest-image` runs *inside* the build. `mkfs.ext4 -d` populates an
image file with no loop device and no privileges, which is what lets the same
Dockerfile be built by CI, by a Linux developer, and by BuildKit inside a
running pool VM (ADR 0062 §6, §7). There is no host-side `fakeroot`, `qemu-img`,
or loop mount to be missing on a machine with no Docker daemon of its own.

It also supplies the three files a container runtime provides and a VM does not
— `/etc/hostname`, `/etc/hosts`, and a `/etc/resolv.conf` that is not the
builder's — and sizes the root filesystem to its contents, because every byte of
slack is a byte pulled over the network on a machine's first pool start.

## Working on it

```
task build:vm-guest     # build and stage where this host's provider finds it
task build:vm-kernel    # libkrun's kernel, Linux only
```

Both stage into the provider's local build directory, which resolution prefers
over the published image when it is complete. Building one is the whole act of
adopting it; deleting the directory is the whole act of going back.

`task vm:build` / `vm:verify` are what CI runs, into `build/vm`. Verification is
not optional politeness: a guest that publishes a kernel the hypervisor cannot
load, or a root filesystem that will not mount, fails as a blank console with
nothing to read.

## Compatibility surface

The guest is released separately from the server, so what passes between them is
a contract:

- the VSOCK port map — 3001 control plane (guest → host), 3002 pool agent, 3003
  lifecycle, 3004 Docker;
- the storage layout — `/dev/vda` root read-only, `/dev/vdb` data, `/dev/vdc`
  cache, and where each is mounted;
- the `discobox-users` virtiofs tag and the path it mounts at;
- `discobox-vsock-guest`, built here from `pool-agent` sources.

Keeping it narrow is what makes independent versioning safe. A change to any of
it is a coordinated release: the guest ships first, then the pin moves.
