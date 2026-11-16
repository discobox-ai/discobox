# 26-11-16-365 — Every Linux base is the newest Ubuntu LTS, read from a fast mirror

- **Status**: Proposed
- **Date**: 2026-11-16
- **Replaces**: the Debian sid pin of #1133 (commits `af3fc836a`,
  `f4685ff80`, `751b3cdd0`) and the trixie exception for Docker of #1164
  (`3fdb360f7`). No ADR recorded either; the root `DESIGN.md` "Debian"
  section is what this replaces.
- **Relates to**: [0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md) §§3, 4, 8
  (one guest image, a distribution kernel where a backend can boot one);
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §10 (the Docker driver's sandbox base);
  [26-11-06-664](26-11-06-664-common-apt-packages-are-built-into-the-docker-base-before-development-binaries.md)
  (the base's apt layer); issue #1227, with a companion change in
  `discobox-ai/vm` (its `pkg/guest/linux`).

## Context

The owner asked (2026-11-16) that the Linux bases move from Debian to the
newest Ubuntu LTS, and that builds use a fast apt mirror rather than Ubuntu's
default hosts, which are often slow.

**Today** every Linux stage this repository builds is Debian sid, from one
image digest (`DEBIAN_IMAGE`, a dated `debian:sid-YYYYMMDD-slim`) and one
`snapshot.debian.org` timestamp (`DEBIAN_SNAPSHOT`), copied into five
Dockerfiles: `vm-image/Dockerfile` (the pool VM guest: `debian`,
`guest-tools`, `rootfs`, `assemble`), `vm-image/kernel/Dockerfile` (the
libkrun kernel's build stage), `server/providers/dockerworker/sandbox-image/Dockerfile`
(every Docker sandbox's and Docker pool's base), and the Go stages of
`sandbox-agent/Dockerfile` and `pool-agent/Dockerfile`, which use sid's
`golang-go` with `GOTOOLCHAIN=local`. `scripts/debian-pin.sh`
(`task check:debian`, `debian:bump`, `docker:bump`) keeps the copies one.
Because a snapshot needs `Check-Valid-Until: no`, each copy writes an apt hook,
`discobox-apt-snapshot`, that refuses an index not dated within the day before
the snapshot. Docker is Docker CE from Docker's **trixie** suite, since Docker
publishes none for sid, on its own pin (`DOCKER_CE_VERSION`,
`CONTAINERD_IO_VERSION`, `DOCKER_APT_KEY_FINGERPRINT`). The sandbox base's
last apt layer hands boxes the live sid archive (`deb.debian.org`). The guest
lifts sid's `linux-image-<arch>` out for vz (arm64: `assemble-guest-image`
decompresses `vmlinuz` to a raw `Image`); libkrun boots libkrunfw's kernel
from `vm-image/kernel`. `task build:ubuntu-sandbox-base` builds a stand-in
base on `ubuntu:24.04` so the features' Ubuntu paths can be exercised.

**What the newest LTS offers**, read 2026-11-16 from Ubuntu's archive:
Ubuntu 26.04 "resolute" (released 2026-04-23). `resolute-updates` carries
`linux-image-virtual`/`-generic` 7.0.0-43, so the guest keeps a 7.x kernel,
one of the reasons sid was chosen. Docker publishes a `resolute` suite
(`download.docker.com/linux/ubuntu`, `docker-ce` up to 29.10.0, 29.9.3 among
them), so the trixie exception is not needed. Dated images exist
(`ubuntu:resolute-20261105.1`). Ubuntu's Go is `golang-1.26-go` 1.26.8; every
`go.mod` here asks for 1.27.1, so the distribution's Go cannot build this
repository.

**Mirrors**, measured from one discobox on 2026-11-16 (indicative, one
location, one afternoon):

| Host | Serves arm64 (`ubuntu-ports`) | Plain http | `resolute-updates/InRelease` (3 runs) | Observed |
| --- | --- | --- | --- | --- |
| `archive.ubuntu.com` / `ports.ubuntu.com` | yes (`ports`) | yes | 0.27–0.90 s / 0.25–1.04 s | one InRelease stalled: 50 KB in 20 s |
| `azure.archive.ubuntu.com` / `azure.ports.ubuntu.com` | yes | yes | 0.11–0.32 s / 0.12–0.75 s | 16 MB index at 2.2 MB/s / 5.6 MB/s |
| `us-east-1.ec2.archive.ubuntu.com` / `…ec2.ports…` | yes | yes | ~0.5 s | 16 MB index at ~0.5 MB/s |
| `mirrors.edge.kernel.org/ubuntu` | **no** (`ubuntu-ports` 404) | https | 0.24–0.29 s | 2.3 MB/s |
| `snapshot.ubuntu.com/ubuntu` | **no** (`ubuntu-ports` answers 401) | **no** (http 301 → https) | 0.35–0.60 s | 7.2 MB/s |

CI builds on Depot runners; boxes run on boxd, on Docker pools on people's
own machines, and on vz/libkrun guests. A mirror must serve both
architectures, from all of those places.

## Decision

### 1. One Ubuntu LTS, resolute, for every Linux base

Every Linux stage this repository builds is Ubuntu 26.04 LTS: the pool VM
guest and the kernel it boots on vz (and, in `discobox-ai/vm`, hcs), the
libkrun kernel's build stage, the sandbox base, and the Go builders. One
release, so one version of everything to maintain, as with sid. No stage
names another distribution or another release, and no stage adds an apt pin
or a PPA; the third-party repositories already there (Docker's, GitHub CLI's)
stay, each held to its key's fingerprint.

The LTS is a deliberate pin, not "whatever is newest": moving to the next
LTS (28.04, April 2028) is a change of its own, made after that release's
first point release, the way this one is. libkrun keeps libkrunfw's kernel;
only its build toolchain is Ubuntu's.

### 2. One pin: the image by digest, and the release's live pockets

Each Dockerfile carries the same values, kept one by a check like today's
(`check:debian` becomes `check:ubuntu`, `debian:bump` becomes `ubuntu:bump`):

- `UBUNTU_IMAGE`: a dated `ubuntu:resolute-YYYYMMDD[.N]` by digest, from ECR
  Public's mirror of Docker's library images, as today.
- apt reads `resolute`, `resolute-updates` and `resolute-security`, components
  `main` and `universe`, from the mirror list of §3, with Ubuntu's archive key
  and **`Check-Valid-Until` on**.

There is **no snapshot**. A rebuild installs the newest packages in the LTS's
pockets, so a security fix (the guest kernel's included) reaches the next
build without a bump, and a scheduled build proves what a release would ship.
The snapshot existed because sid moves daily and can catch a transition half
done; an LTS's `-updates` and `-security` take only Stable Release Updates, so
the reason for it is gone, and so are its costs: a slow single host,
`Check-Valid-Until: no`, and the replay hook `discobox-apt-snapshot` that
made up for it. The check refuses a snapshot source, another release name, a
pin, and a source that is not the mirror list.

What a rebuild cannot reproduce is recorded instead: the guest artifact
carries the `dpkg-query` package list it was built from, beside the kernel
config the kernel artifact already carries, and every Ubuntu stage prints the
mirror it fetched from in the build log.

### 3. A fast mirror first, Ubuntu's own as the fallback, in one list

apt reads through its `mirror+file:` method, from one list per architecture
written by every Ubuntu stage:

- amd64: `http://azure.archive.ubuntu.com/ubuntu`, then
  `http://archive.ubuntu.com/ubuntu`.
- arm64: `http://azure.ports.ubuntu.com/ubuntu-ports`, then
  `http://ports.ubuntu.com/ubuntu-ports`.

apt tries the next entry when a fetch from one fails, so a mirror outage
costs time, not a build. How much time was measured on `ubuntu:resolute`
(2026-11-16), with a first entry that drops every packet and
`Acquire::http::Timeout=5`: `apt-get update` succeeded from the second entry
but took 5 min 29 s, because apt gives up on the dead host again for every
file; a host that refuses or answers an error moves on at once. So every
stage sets a short connect timeout for the list, and a mirror that stays
down is taken out of the pin, not left to the fallback. Every entry is plain http: the image has no CA
certificates until apt installs them, and trust comes from the signed
`InRelease`, which with §2's `Check-Valid-Until` on also refuses a stale
index, whatever host served it. `Acquire::Retries "5"` stays.

The mirror is one value per architecture, in the same pin and the same
check, so changing it is one edit and a `ubuntu:bump`. Canonical's Azure
mirror is chosen because, of the candidates, it is the one that serves both
architectures over http and was fast from here; this is evidence from one
place, so the implementation measures it from CI (Depot) and from a boxd
box against the defaults, and records the build-time difference on #1227
(see Open).

### 4. Boxes keep the same sources

The sandbox base and the guest keep the §3 sources as built, so an
`apt install` in a box, and every feature provisioned onto it, reads the fast
mirror with Ubuntu's archive behind it. The handback step that rewrote
sources to `deb.debian.org` and removed the snapshot hook goes: there is
nothing to hand back. A box on another provider's image (exe.dev, boxd) keeps
that image's sources; this decision is about the images this repository
builds.

### 5. Docker CE from Docker's resolute suite, on the pin it has

Docker is Docker CE from `download.docker.com/linux/ubuntu`, suite `resolute`,
in the sandbox base and the guest. `DOCKER_DEBIAN_SUITE` and its trixie value
go; the suite is the LTS's codename. The Docker pin
(`DOCKER_CE_VERSION`, `CONTAINERD_IO_VERSION`, `DOCKER_APT_KEY_FINGERPRINT`)
stays, and still moves on its own clock with `docker:bump`: it is what makes a
pool's daemon the one its sandboxes run and lets a bad Docker release be held
back, and neither reason was about Debian. Ubuntu's `docker.io` is not used.

### 6. Go is upstream's, on one pinned version

The Go stages of `sandbox-agent/Dockerfile`, `pool-agent/Dockerfile` and
`vm-image/Dockerfile` (`guest-tools`) install Go's upstream release, by
version and per-architecture SHA-256, as the sandbox base already does
(`GO_VERSION`, `GO_SHA256_AMD64`, `GO_SHA256_ARM64`). Those values become one
pin across every copy, in the same check, with `GOTOOLCHAIN=local` kept so a
`go.mod` newer than the pin fails the build rather than fetching a toolchain.
Moving it is `go:bump` (or part of `ubuntu:bump`; see Open). There is no
`golang` image on Ubuntu, and the official `golang` images are Debian, so
neither is used.

### 7. The guest boots Ubuntu's virtual kernel

The guest installs `linux-image-virtual` from `resolute-updates`, the kernel
vz boots (and hcs, in `discobox-ai/vm`). `assemble-guest-image` still
produces a raw arm64 `Image` from `vmlinuz` and fails if no known compression
yields one; the guest's assembly checks (lockup detectors) and `vm:verify`'s
module list (netfilter, conntrack, bridge, overlay, VSOCK, virtio) still
apply, unchanged, to the new kernel. The other sid workarounds
(`zstd` for initramfs compression, masking `systemd-logind-varlink.socket`)
are kept only where the Ubuntu build is shown to need them.

### 8. Scope across the two repositories

This repository's five Dockerfiles, its `scripts/debian-pin.sh` (renamed for
what it now checks), the Taskfile targets, the workflows' comments, and the
design and review notes move together. `discobox-ai/vm` moves its guest
(`pkg/guest/linux`: rootfs, assemble, krun-kernel stages), its Rust stage and
its e2e and test images in a companion change, on the same LTS, the same
mirror list and the same rules; its own pin is its own, as the Debian
snapshot was.

## Consequences

- **Builds and boxes stop depending on snapshot.debian.org**, the slowest and
  least available host they used, and on any single Ubuntu host.
- **Rebuilds are no longer byte-for-byte reproducible.** The same commit built
  a week apart can install newer `-updates`/`-security` packages. What
  shipped stays readable (the guest's package list, the image digest), and
  every shipped artifact is still pinned by digest wherever it is consumed.
  A bad SRU breaks the next build rather than waiting for a bump; the answer
  is to hold that one package back explicitly, in the open, until it is fixed.
- **Security fixes arrive without a bump.** A guest or sandbox base built
  today has today's fixes; `ubuntu:bump` is only for the base image's digest
  (which apt would otherwise upgrade on top of anyway).
- **`task build:ubuntu-sandbox-base` goes**: the base is Ubuntu. The
  features' `debian:` package lists and `distro: [debian]` steps stay, for a
  Debian image a pool is pointed at, but nothing here builds one, so they
  are exercised only by their tests.
- **Ubuntu's `ubuntu` account (uid 1000)** is in the image; the sandbox base
  already makes it give way to the sandbox's account.
- **A guest and kernel release** follow, like any Debian bump did; the
  release's pin check (`discobox-vm-pins`) sees the change to `vm-image/`.

## Alternatives rejected

- **Keep Debian sid.** The owner asked to leave it.
- **Debian stable (trixie).** Not what was asked, and its kernel is 6.x.
- **Ubuntu's snapshot service, for reproducibility as today.**
  `snapshot.ubuntu.com` answers only over https, which the bare image cannot
  check before apt installs `ca-certificates` (the same bootstrap sid had),
  and its `ubuntu-ports` tree, which arm64 (vz's guest, a Mac's Docker pool)
  needs, answered 401 on 2026-11-16. A snapshot for amd64 alone would split
  the bases by architecture. Revisit if Canonical serves `ubuntu-ports`
  snapshots publicly and over http, and reproducible rebuilds become a
  requirement again.
- **Ubuntu's default hosts** (`archive`, `security`, `ports.ubuntu.com`). The
  owner's explicit "no"; they stay only as the fallback.
- **`mirrors.edge.kernel.org`, or another mirror without `ubuntu-ports`.**
  Fast for amd64, nothing for arm64; two mirror families would be two
  things to keep working.
- **A cloud region's mirror (`*.ec2.archive.ubuntu.com`).** Fast inside its
  region and slow outside it (0.5 MB/s here); boxes run everywhere.
- **Our own apt cache or mirror** (a pull-through cache on the pool, or a
  hosted mirror). It would be fastest inside a pool and the pool already
  caches bundles, but it is a service to run and secure for a gain the public
  mirror may already give. Revisit if the measurements in Open show the
  chosen mirror is the bottleneck of a build or of provisioning.
- **Ubuntu's Go, or a `golang` image.** The LTS's Go (1.26) is older than
  what `go.mod` asks for; the `golang` images are Debian.

## Open, for implementation

- Confirm the mirror choice by measurement from CI and a boxd box (and, if
  available, a Docker pool on a Mac): build time of the sandbox base and the
  guest, and `apt-get update && apt-get install` time in a box, before and
  after, recorded on #1227. If the Azure mirror is not the fastest of the
  candidates that serve both architectures over http, the other one is used:
  the decision is the shape (one fast mirror, Ubuntu's behind it, one value
  per architecture), not the host name.
- Whether `linux-image-virtual`'s modules cover the guest's list, or the
  guest needs `linux-modules-extra` too; `vm:verify` decides.
- Whether the Go pin is moved by its own task or by `ubuntu:bump`.
- What `discobox-ai/vm`'s Rust stage takes: Ubuntu's `rustc`, or an upstream
  toolchain pinned like Go.
- The connect timeout's value, and whether apt can be made to skip a dead
  entry for the rest of a run rather than once per file.
