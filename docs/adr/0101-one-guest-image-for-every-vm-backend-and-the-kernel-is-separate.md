# 0101 — One guest image for every VM backend, and the kernel is a separate artifact

- **Status**: Accepted
- **Date**: 2026-09-04

## Context

[ADR 0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md)
§9 says libkrun converges on the macOS backend's style: it adopts the
independently released guest image (§3), the `guestimage` resolver (§5), and the
one-Dockerfile artifact set (§6), and its Rust launcher becomes a hidden
subcommand of the server binary. That is the work this ADR sits inside.

ADR 0062 deferred one thing explicitly:

> **One guest image for both `vz` and libkrun.** Revisit once §9 has landed and
> the two images differ only in network mode; converging before then would be
> the coupling §5's alternative rejects.

The condition is now inspectable rather than hypothetical, because §9 is what
removes every other difference. Before it, the two images differed in
filesystem format (QCOW2 against raw ext4), in how they were assembled (a host
`fakeroot` script against a `RUN` step), in who formatted the data disks (the
host's `mkfs.ext4` against the guest), and in the boot artifact set. §9 deletes
all four by construction: libkrun's root becomes a raw ext4 image assembled
inside the build, and its disks are created sparse and formatted by the guest,
because that is what §6 and the shared `guestimage` resolver mean.

What is left is two differences, and they are not the same kind of thing.

**Networking.** `vz` takes its address from the DHCP server in
Virtualization.framework's NAT attachment. libkrun's guest configured a fixed
private address, route, and resolver by hand, through a `discobox-network`
oneshot and a static `/etc/resolv.conf`, because ADR 0013 described passt as
supplying "the guest's conventional virtio-net interface" without a resolver
daemon. But passt runs a DHCP server of its own, and the launcher already tells
it exactly what to hand out — `--address`, `--gateway`, `--dns`. The static
configuration was restating passt's own lease inside the guest.

**The kernel.** libkrun boots a kernel directly through libkrunfw's patched
entry paths, so no distribution kernel boots under it at all; the project builds
a patched Linux for this and nothing else. `vz` is stock virtio and takes
Debian's kernel, which is exactly why ADR 0062 §8 rejected building one. This
difference is not going away, and it is not a difference in the *guest*: it is a
difference in what the hypervisor will load.

The two are worth separating because they pull in opposite directions. One is a
difference the guest should not have. The other is a difference the guest should
not carry.

## Decision

### 1. There is one guest image, built from one Dockerfile

`vm-image/Dockerfile` produces the boot artifact set for every VM backend, at
the repository root beside `base-image/` because it belongs to no provider. It
is built for `linux/arm64` and `linux/amd64` from the same source with no
per-backend branch in it, and published as one multi-architecture
`discobox-vm`. `vz` boots the arm64 variant; libkrun boots the amd64 one.

The pin moves with it. `guestimage.DefaultVMImage` is the one digest, because
one publish has to be one edit — two constants are how a backend ends up quietly
booting the release before last.

### 2. The guest takes its address from DHCP, whatever runs it

One `systemd-networkd` unit, `DHCP=ipv4`, resolving through `systemd-resolved`.
passt and Virtualization.framework's NAT both serve DHCP, so this is the
configuration both hosts can drive, and the guest never learns which hypervisor
it woke up on.

It matches `en* eth*`, and both halves are load-bearing: a virtio-pci NIC gets a
predictable `enp0s*` name, while libkrun's virtio-mmio NIC has no topology to
derive one from and keeps `eth0`. This is the one place the two backends were
not already alike underneath the configuration.

libkrun's `discobox-network` unit, `discobox-configure-network`, and static
`resolv.conf` are removed. The addressing itself does not change and does not
move: it is still the launcher's `--address`/`--gateway`/`--dns` arguments to
passt, which is the process that owns it.

`vm:verify` asserts the built root filesystem takes DHCP, because this is the
one property whose absence would leave the image bootable on exactly one of the
two backends — and the backend it broke would fail with a guest that comes up,
runs dockerd, and answers nothing.

### 3. The libkrun kernel is its own artifact on its own release line

`vm-image/kernel/Dockerfile` builds the libkrunfw-patched kernel and publishes
it as `discobox-vm-kernel` (linux/amd64) under `vm-kernel/v*` tags, from its own
workflow. The libkrun provider resolves it with a second `guestimage.Resolver`
and pins it in `DefaultKernelImage`.

The workflow is separate for the same reason the artifact is: this build
compiles Linux, and a guest change must not pay for it.

It is separate rather than a stage of the guest image because the inputs move on
unrelated clocks — this changes when libkrunfw or upstream Linux does, the guest
when Debian or Docker does — and because folding them together would make every
guest rebuild compile a kernel and every kernel bump republish a userland. It is
the same argument ADR 0062 §3 makes for splitting the guest from the product,
applied one level down.

### 4. The shared image still publishes a kernel, and libkrun ignores it

`linux-image-<arch>` is installed on both architectures, so the Dockerfile has
no conditional in it. Assembly publishes `vmlinux` only where it can produce an
uncompressed image the hypervisor will load, which today is arm64; amd64
publishes `root.ext4` and `initrd.img` and says in its output why there is no
kernel beside them.

libkrun asks the resolver for `root.ext4` and nothing else, so the artifacts it
cannot use are never extracted to a machine that would not boot them.

### 5. `guestimage` refuses an image built for another architecture

`remote.WithPlatform` selects a child of an index and does nothing at all to a
single-architecture manifest, which is returned whatever was asked for. With one
image name now carrying two architectures, that is a live failure mode with no
symptom worth reading: the artifacts extract, the VM starts, and the guest
panics on its first instruction.

The image's own config is checked against the requested platform. Only a stated
mismatch is refused — an image declaring no architecture makes no claim to
contradict, and rejecting it would reject a hand-assembled artifact set that
boots.

## Consequences

- The guest image's blast radius is both backends. A change to `vm-image/` that
  breaks the guest breaks macOS and Linux pools together, where before it broke
  one. That is the cost being bought: the compensation is that `vm:verify` runs
  on both architectures in CI, on native runners, before anything is published.
- `vz` gains an amd64 sibling it does not use and libkrun gains an arm64 one,
  each about a hundred megabytes in the registry. Neither is ever pulled by a
  host that cannot boot it, because resolution is by platform.
- libkrun's guest now runs `systemd-networkd` and `systemd-resolved`, which it
  previously did not. That is two more units in a guest whose scope rule
  (ADR 0062 §4) is "Docker's host and nothing else" — they are there because
  bringing an interface up is inside that scope, and they are what makes one
  image possible.
- The amd64 guest carries Debian's kernel modules for a kernel it never boots.
  Nothing depends on them: the VSOCK transport is loaded by the units that need
  it, with failure ignored, so a kernel that has it built in boots no failed
  unit — and the clock timer is skipped outright where there is no host-backed
  RTC rather than writing to the serial console every thirty seconds. Both were
  per-backend assumptions that only became visible once one image had to serve
  two.
- There are now three release lines to operate: the product, `vm/v*`, and
  `vm-kernel/v*`. The compatibility surface between them is the VSOCK port map,
  the storage layout, the `discobox-users` share tag, and `discobox-vsock-guest`
  — the same narrow surface ADR 0062 §3 named, not a wider one.
- ADR 0062's deferred "one guest image" item is settled. Its Rosetta and Intel
  Mac deferrals are untouched.

## Alternatives

**Keep two Dockerfiles publishing into one image name.** Rejected: it takes the
cost of a shared name — one pin, one release line, one place to look — without
the benefit, and leaves two guests to keep in step by hand. The name would then
be the only thing shared, which is the worst of both.

**One Dockerfile with the network selected by build arg or target architecture.**
Rejected: the divergence it preserves is not real. passt serves DHCP, so the
static configuration was a second spelling of the lease passt already hands out,
and a conditional would have kept a per-backend branch in the one file whose
whole point is not having one.

**Put the libkrun kernel in the guest image for amd64.** Rejected per §3: the
clocks are wrong for it, and it would make the Dockerfile conditional on the
architecture in the one place §1 exists to avoid.

**Publish the guest as two images, `discobox-vm-vz` and `discobox-vm-krun`.**
Rejected: they would be the same bytes for the same architecture, differing only
in who pulls them, and two pins to move after one build.

**Leave the convergence deferred and land §9 alone.** Rejected as the more
expensive order. §9 rewrites libkrun's image assembly, its disk lifecycle, and
its artifact resolution; converging afterwards would rewrite the same files a
second time, and in between there would be two guest images that are byte-for-
byte alike apart from a network unit — which is exactly the state a reviewer
would ask why anybody chose.
