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
  the one this repository owns. systemd's preset enables the unit as well
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
  decides only whether the distribution's kernel can be published in a form a hypervisor
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
- **Docker is Docker CE at the shared pin, never a distribution's `docker.io`.** The guest's
  daemon is the sandbox base's: same repository, same `DOCKER_CE_VERSION` and
  `CONTAINERD_IO_VERSION` (`check:ubuntu` keeps the copies one). Debian's build
  of 28.5.2 refused archive copies Docker's own builds accept, and a pool's box
  is placed by one (#1164). Moving the version is `go tool task docker:bump`,
  then a guest release, as for an Ubuntu bump. Docker's key is held to
  `DOCKER_APT_KEY_FINGERPRINT`; never take it on first download, because the
  server that serves it serves the packages too.
- **A libkrun guest's poweroff ends its VM only through the kernel's patch.**
  `kernel/patches` makes an x86 power-off reset the CPU, which is the one exit
  libkrun has. Don't move that into the guest image (a shutdown hook that
  reboots under libkrun): the guest image boots on every backend and must not
  name one. A libkrunfw or kernel bump must still apply the patch: the
  build fails if it doesn't, and `vm:verify-kernel` fails on a `vmlinux`
  without the handler. That check finds it by its symbol, `krun_power_off`, so
  renaming the function in the patch means changing the Taskfile check too.
- **Guest image changes are a separate release.** Editing this directory does
  not ship with the server; it ships when a `vm/v*` tag is cut and
  `guestimage.DefaultVMImage` is re-pinned to the new `discobox-vm` digest.
  `kernel/` ships on its own line the same way: a `vm-kernel/v*` tag, then
  libkrun's `DefaultKernelImage` is re-pinned to the new `discobox-vm-kernel`
  digest. Until both are, the next product release is refused
  (`release:check-vm-pins`), so a change here is not done when it merges.
- **A build input is listed twice; change both.** The paths filter of
  `vm-image.yml` (or `vm-kernel.yml`) and the pin's `Paths` in
  `internal/cmd/discobox-vm-pins` must name the same inputs: a `COPY` of a new
  directory into either Dockerfile goes in both, or the release passes a pin
  that lacks it. A new pinned VM release line, or a moved or renamed pin
  constant (`vm:publish`'s `PUSH_FILE`/`PUSH_CONST`), goes in that tool's
  `pins` table too: it fails on a constant it cannot find, but never learns
  of a line it was not told about.
- **Do not replace the clock step with NTP, or make it conditional on anything
  but the RTC's presence.** A guest is hours off precisely when its host has
  slept, which is the case an NTP daemon refuses to correct on its own. Every
  401 in both directions traces back here. It is already conditioned on the
  host-backed RTC existing, which is what keeps it quiet where there is none.
- **Assembly runs inside the build.** `mkfs.ext4 -d` needs no loop device and no
  privileges, which is what lets CI, a Linux developer, and BuildKit inside a
  running pool VM all produce the same artifacts. A host-side step reintroduces
  the dependency ADR 0062 §6 removed, and breaks the macOS build loop outright.
- **The classic-store cleanup splits shared directories by store.**
  `discobox-mount-storage` removes what a classic-store guest left on the data
  disk, but `containers/` and `buildkit/` under Docker's root are written by
  both stores. Move only the entries that are the classic store's (a container
  whose driver is `overlay2`; BuildKit's classic-only files), never the whole
  directory: a containerd-store disk that also still has classic data would
  lose its live containers, or orphan BuildKit snapshots that containerd's
  leases still hold and nothing can prune. Which files are classic-only
  depends on the Docker this guest installs, not on current moby: Debian's
  Docker 26 wrote `buildkit/history.db` in both stores, while 28 and Docker
  CE 29 name the containerd store's `history_c8d.db`, leaving `history.db`
  the classic store's (CE 29.9.3's containerd store writes `cache.db`,
  `containerd-overlayfs/`, `executor/` and `history_c8d.db`). Check a list
  change, and any `docker:bump` that moves the guest's Docker, against a
  guest booted on each store.
- **The guest's Ubuntu is the repository's one pin.** Every stage is
  resolute, from `UBUNTU_IMAGE`, reading the mirror list; never add an apt
  pin, another release, a PPA, a snapshot, or another base to take one
  package from elsewhere. Docker CE (Docker's resolute suite) and Go
  (upstream's) are on pins of their own. A bad update in the pockets is held
  back by naming the working version (`pkg=<version>`) in the install line,
  with a comment naming the bug (see "Ubuntu" in the root `DESIGN.md`).
  `check:ubuntu` catches a
  literal base, another release named in a build, an apt pin, and a source
  other than the mirror list, but not a stage that fetches packages some
  other way (a `.deb` by URL, a tarball): that is still review's to catch.
  Bumping the image is a guest release: `vm:verify` on both architectures and
  a boot on each backend.
- **The mirror list is what makes a fast mirror safe; keep every copy one.**
  Every entry is plain http, so trust is the archive's signature and the
  index's age. Ubuntu's Release files carry no `Valid-Until`, so
  `Check-Valid-Until` alone protects nothing: the `-updates`/`-security`
  stanza's `Valid-Until-Max` is what refuses a replayed old index. Never drop
  it, never turn `Check-Valid-Until` off, and never add an https entry the
  bare image cannot verify before `ca-certificates` is installed. The list's
  setup is written out in every stage on the pin; `check:ubuntu` fails when a
  copy differs, so a fix goes into all of them.
