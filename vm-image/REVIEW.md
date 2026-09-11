# Pool VM Guest Image Review Rules

One image boots every VM backend, so a mistake here is not one backend's. By
ADR 0101's own consequences, breaking this "breaks macOS and Linux pools
together, where before it broke one".

- **Do not mask `systemd-networkd-wait-online.service`.** It is the only thing
  that makes `network-online.target` mean anything under networkd, and
  `docker.service` orders itself after that target. Masking it does not remove
  the dependency, it silently satisfies it, and `dockerd` then races the DHCP
  lease this guest takes its address, route, *and* resolver from — a race it
  loses often enough to fail the pool's first registry pull and mark the pool
  offline. It belongs with the units the guest enables, not with the ones a
  headless guest masks. Both backends take a lease, so this is not vz's alone.
  The explicit `systemctl enable` in the Dockerfile is the guarantee, and it is
  the one this repository owns. Debian's preset enables the unit as well
  (`90-systemd.preset`), so deleting that line builds clean and changes nothing
  observable today — which is exactly the trap. It hands enablement to a third
  party, and the day the preset drops the unit it is a silent regression on both
  backends. Keep the line; treat the preset as redundancy, not as the switch.

  Masking is the other way to lose it, and `systemctl enable` refuses a masked
  unit, so masking it while that line stands fails the build outright.

  `vm:verify` asserts both that the unit is unmasked and that it is linked into
  `network-online.target.wants`. The second earns its place independently: it
  observes what the artifact ends up with, however it got there — including a
  preset that changed under a Dockerfile nobody edited.
- **Nothing here may name a backend.** No build arg or `TARGETARCH` branch that
  selects backend behaviour, and no unit that only one hypervisor can satisfy.
  The one architecture branch is `assemble-guest-image`'s kernel case, and it
  decides only whether Debian's kernel can be published in a form a hypervisor
  loads directly (ADR 0101 §4). It never changes the root filesystem. A
  difference a backend genuinely needs and the other cannot take belongs in a
  separate artifact with its own release line, as the libkrunfw-patched kernel
  does (ADR 0101 §3). The one
  thing that could have differed — how the guest gets an address — is answered
  the same way for both, because passt serves DHCP as Virtualization.framework's
  NAT attachment does.
- **A unit that fails on one backend is masked or conditioned, not tolerated.**
  The serial console is the only diagnostic a guest that never came up leaves
  behind, so a permanently failed unit spends the signal that tells the next
  person something is actually wrong. `/Users` is the single exception, and
  `vm-image/DESIGN.md` says so.
- **Disk order is a contract.** Root, data and cache are `vda`, `vdb`, `vdc`,
  and `discobox-mount-storage` in this directory mounts the last two by those
  names. Both drivers hard-code the order against it: `vzvm`'s
  `storageDevices`, and `krunvm`'s fixed `krun_add_disk3` sequence. Reordering,
  inserting a disk, or changing the script to mount by label silently mounts
  the wrong filesystem on both backends at once.
- **A new guest device is three edits, not one.** The `Dockerfile` deletes
  whole kernel driver classes that virtio hardware never needs, and builds the
  initrd from an explicit module list. So attaching a device the guest has never
  had is three edits. Attach it in the drivers. Keep its driver class off the
  `Dockerfile`'s delete list. If the root filesystem depends on it, add it to
  the list the `Dockerfile` writes to `/etc/initramfs-tools/modules`. A missing
  module is a guest that boots to no device or does not boot at all.
- **Guest image changes are a separate release.** Editing this directory does
  not ship with the server; it ships when a `vm/v*` tag is cut and
  `guestimage.DefaultVMImage` is re-pinned to the new `discobox-vm` digest.
  `kernel/` ships on its own line the same way: a `vm-kernel/v*` tag, then
  libkrun's `DefaultKernelImage` is re-pinned to the new `discobox-vm-kernel`
  digest.
- **Do not replace the clock step with NTP, or make it conditional on anything
  but the RTC's presence.** A guest is hours off precisely when its host has
  slept, which is the case an NTP daemon refuses to correct on its own. Every
  401 in both directions traces back here. It is already conditioned on the
  host-backed RTC existing, which is what keeps it quiet where there is none.
- **Assembly runs inside the build.** `mkfs.ext4 -d` needs no loop device and no
  privileges, which is what lets CI, a Linux developer, and BuildKit inside a
  running pool VM all produce the same artifacts. A host-side step reintroduces
  the dependency ADR 0062 §6 removed, and breaks the macOS build loop outright.
