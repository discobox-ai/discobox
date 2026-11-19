# The Discobox pool VM guest image.
#
# One guest for every VM backend. `vz` boots it on Virtualization.framework
# (linux/arm64) and `libkrun` boots it on KVM (linux/amd64); nothing in here
# names either of them, and the two arches are the same build.
#
# The guest is Docker's host and nothing else: everything Discobox does runs in
# containers on the daemon this image boots. Nothing that is not needed to bring
# dockerd and the two VSOCK helpers up belongs here.
#
# The build produces the boot artifact set in its final stage — a kernel, an
# initrd, and a raw ext4 root filesystem — so the same Dockerfile is buildable
# by CI, by a Linux developer, and by BuildKit inside a running pool VM
# (ADR 0062 §6, §7). Filesystem assembly runs in a RUN step: mkfs.ext4 -d
# populates an image file with no loop device and no privileges, so no
# host-side fakeroot or qemu-img step exists to be missing on a Mac.
#
# The kernel is the one thing a backend may not take from here. libkrun needs
# the libkrunfw-patched kernel, which is a separate artifact with its own
# release line (kernel/Dockerfile), and it ignores the one this build lifts out
# of the distribution package.
#
# Every stage is Ubuntu 26.04 LTS (resolute): the image pinned by digest, apt
# reading the release's live pockets from the ordered mirror list for its
# architecture. They are the repository's one Ubuntu pin, the same values
# in every Dockerfile that builds on Ubuntu; see "Ubuntu" in the root
# DESIGN.md, and `go tool task ubuntu:bump` to move the image. Docker CE (from
# Docker's resolute suite) and Go (upstream's release) have pins of their own
# (DOCKER_CE_VERSION and GO_VERSION below, moved by `go tool task docker:bump`
# and `go tool task go:bump`).
ARG UBUNTU_IMAGE=public.ecr.aws/docker/library/ubuntu:resolute-20261105.1@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db
ARG UBUNTU_MIRROR_AMD64=http://mirrors.edge.kernel.org/ubuntu
ARG UBUNTU_MIRROR_ARM64=http://mirrors.mit.edu/ubuntu-ports
ARG GO_VERSION=1.27.3
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49

# The stages that run on the target architecture start here, with apt reading
# the mirror list. Every entry is plain http: the image has no CA certificates
# to check TLS with before apt installs them, and apt verifies every index
# against Ubuntu's archive key, whichever host served it. Ubuntu's Release
# files carry no Valid-Until, so the moving pockets (-updates, -security) get
# Valid-Until-Max: apt refuses one dated more than a week ago, so no host can
# replay an older, validly signed index of them. The frozen release pocket is
# dated its release day and stays out of that. The Go stage below and every
# Dockerfile on the Ubuntu pin write the same list.
FROM ${UBUNTU_IMAGE} AS ubuntu
ARG UBUNTU_MIRROR_AMD64
ARG UBUNTU_MIRROR_ARM64
RUN set -eu \
    && case "$(dpkg --print-architecture)" in \
        amd64) first=$UBUNTU_MIRROR_AMD64 second=http://ftp.osuosl.org/pub/ubuntu third=http://azure.archive.ubuntu.com/ubuntu ;; \
        arm64) first=$UBUNTU_MIRROR_ARM64 second=http://azure.ports.ubuntu.com/ubuntu-ports third=http://ports.ubuntu.com/ubuntu-ports ;; \
        *) echo "no Ubuntu mirror for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && mkdir -p /etc/apt/mirrors \
    && printf '%s\tpriority:1\n%s\tpriority:2\n%s\tpriority:3\n' "$first" "$second" "$third" > /etc/apt/mirrors/ubuntu.list \
    && printf '%s\n' 'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' '' \
        'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute-updates resolute-security' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' 'Valid-Until-Max: 604800' \
        > /etc/apt/sources.list.d/ubuntu.sources \
    && printf '%s\n' 'Acquire::Retries "5";' 'Acquire::http::Timeout "5";' > /etc/apt/apt.conf.d/80-discobox-apt \
    && echo "apt reads $first, then $second, then $third"

# Go is upstream's, on the repository's one Go pin: the LTS's Go is older than
# go.mod asks for, and the golang images are Debian. Built natively on the host
# platform and cross-compiled, so nothing is emulated.
FROM --platform=$BUILDPLATFORM ${UBUNTU_IMAGE} AS guest-tools
ARG UBUNTU_MIRROR_AMD64
ARG UBUNTU_MIRROR_ARM64
ARG GO_VERSION
ARG GO_SHA256_AMD64
ARG GO_SHA256_ARM64
ARG TARGETOS=linux
ARG TARGETARCH

RUN set -eu \
    && case "$(dpkg --print-architecture)" in \
        amd64) first=$UBUNTU_MIRROR_AMD64 second=http://ftp.osuosl.org/pub/ubuntu third=http://azure.archive.ubuntu.com/ubuntu ;; \
        arm64) first=$UBUNTU_MIRROR_ARM64 second=http://azure.ports.ubuntu.com/ubuntu-ports third=http://ports.ubuntu.com/ubuntu-ports ;; \
        *) echo "no Ubuntu mirror for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && mkdir -p /etc/apt/mirrors \
    && printf '%s\tpriority:1\n%s\tpriority:2\n%s\tpriority:3\n' "$first" "$second" "$third" > /etc/apt/mirrors/ubuntu.list \
    && printf '%s\n' 'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' '' \
        'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute-updates resolute-security' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' 'Valid-Until-Max: 604800' \
        > /etc/apt/sources.list.d/ubuntu.sources \
    && printf '%s\n' 'Acquire::Retries "5";' 'Acquire::http::Timeout "5";' > /etc/apt/apt.conf.d/80-discobox-apt \
    && echo "apt reads $first, then $second, then $third"
RUN cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu \
    && arch="$(dpkg --print-architecture)" \
    && case "$arch" in \
        amd64) go_sha=$GO_SHA256_AMD64 ;; \
        arm64) go_sha=$GO_SHA256_ARM64 ;; \
        *) echo "no Go for $arch" >&2; exit 1 ;; \
    esac \
    && curl -fsSL --retry 5 --retry-all-errors -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" \
    && echo "$go_sha  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz \
    && /usr/local/go/bin/go version
# The pinned Go is the Go that builds: a go.mod that asks for a newer one
# fails here, rather than fetching a toolchain nothing pins. Moving it is
# `go tool task go:bump`.
ENV PATH=/usr/local/go/bin:${PATH} GOPATH=/go GOTOOLCHAIN=local

WORKDIR /src
COPY go.mod go.sum ./
COPY pool-agent/go.mod pool-agent/go.sum ./pool-agent/
COPY pkg/vsock ./pkg/vsock
COPY pool-agent/cmd/discobox-vsock-guest ./pool-agent/cmd/discobox-vsock-guest
WORKDIR /src/pool-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/discobox-vsock-guest ./cmd/discobox-vsock-guest

# Docker's apt key, fetched where curl, CA certificates and gpg are, so none of
# them has to be in the guest to read Docker's repository. The key is served
# beside the packages it signs, so it is held to Docker's published
# fingerprint: a key that is not Docker's fails the build, rather than signing
# the daemon every pool runs as root.
FROM ubuntu AS docker-apt-key
ARG DOCKER_APT_KEY_FINGERPRINT=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
RUN cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gpg \
    && curl -fsSL --retry 5 --retry-all-errors https://download.docker.com/linux/ubuntu/gpg -o /docker.asc \
    && fingerprints=$(gpg --show-keys --with-colons /docker.asc | awk -F: '$1 == "pub" { primary = 1 } $1 == "fpr" && primary { print $10; primary = 0 }') \
    && if [ "$fingerprints" != "$DOCKER_APT_KEY_FINGERPRINT" ]; then \
        echo "Docker's apt key is not $DOCKER_APT_KEY_FINGERPRINT: $fingerprints" >&2; exit 1; \
    fi \
    && chmod a+r /docker.asc

FROM ubuntu AS rootfs
ARG TARGETARCH

ENV container=discobox-vm

# Documentation is dead weight in an artifact whose size is a distribution
# cost, and dpkg is the only thing that ever puts it here.
RUN printf '%s\n' \
        'path-exclude /usr/share/doc/*' \
        'path-exclude /usr/share/info/*' \
        'path-exclude /usr/share/locale/*' \
        'path-exclude /usr/share/man/*' \
        > /etc/dpkg/dpkg.cfg.d/01-discobox-nodoc

# linux-image-virtual is the only reason this image is not tiny, and it is what
# removes a kernel build from the backends that can use it: a hypervisor taking
# stock virtio needs no patched kernel (ADR 0062 §8). It is installed on every
# architecture so this build has no per-backend branch in it, and a backend that
# brings its own kernel simply does not ask for the one assembly lifts out.
#
# It is Ubuntu's virtual kernel from resolute-updates, because the root is
# resolute, the repository's one Ubuntu, with no apt pin taking it from
# anywhere else; a rebuild takes the newest the pocket has. initramfs-tools is
# named because Ubuntu's default initramfs generator is dracut, and the initrd
# below is built from initramfs-tools' module list. zstd is listed because
# initramfs-tools does not pull it in, and without it COMPRESS=zstd below
# quietly falls back to gzip.
RUN cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        dbus \
        e2fsprogs \
        initramfs-tools \
        iproute2 \
        iputils-ping \
        linux-image-virtual \
        systemd \
        systemd-resolved \
        systemd-sysv \
        util-linux \
        zstd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Docker is Docker CE from Docker's own repository, its resolute suite, the
# Docker the sandbox base runs, at the same version: DOCKER_CE_VERSION and
# CONTAINERD_IO_VERSION are one pin with the sandbox base's, kept one by
# `go tool task check:ubuntu` and moved by `go tool task docker:bump`.
# Not a distribution's docker.io: Debian's build of Docker 28.5.2 refused every
# archive copy into a container whose bind crosses the /var/run link, where
# Docker's own builds accept it (#1164). A pin names the upstream version and
# the build takes the suite's package of it, so a rebuild installs the same
# Docker even though Docker's repository is not pinned.
#
# docker-ce depends on docker-ce-cli, and nothing in the guest runs the CLI
# (the pool's console brings its own), so it goes.
ARG DOCKER_CE_VERSION=29.9.3-1
ARG CONTAINERD_IO_VERSION=2.4.2-4
COPY --from=docker-apt-key /docker.asc /etc/apt/keyrings/docker.asc
RUN . /etc/os-release \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
        "$VERSION_CODENAME" "$(dpkg --print-architecture)" \
        > /etc/apt/sources.list.d/docker.sources \
    && cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && ce=$(apt-cache madison docker-ce | awk -v v="5:${DOCKER_CE_VERSION}~" 'index($3, v) == 1 { print $3; exit }') \
    && containerd=$(apt-cache madison containerd.io | awk -v v="${CONTAINERD_IO_VERSION}~" 'index($3, v) == 1 { print $3; exit }') \
    && if [ -z "$ce" ] || [ -z "$containerd" ]; then \
        echo "Docker's $VERSION_CODENAME suite has no docker-ce $DOCKER_CE_VERSION or containerd.io $CONTAINERD_IO_VERSION" >&2; exit 1; \
    fi \
    && apt-get install -y --no-install-recommends \
        containerd.io="$containerd" \
        docker-ce="$ce" \
        docker-ce-cli="$ce" \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /usr/bin/docker \
    && rm -rf /usr/libexec/docker/cli-plugins

# Ubuntu's kernel packages carry a module for every device Ubuntu supports.
# This guest's entire hardware surface is virtio — block, net, vsock, console,
# entropy, balloon, fs — whichever hypervisor attaches it, so whole driver
# classes here describe hardware that cannot appear. depmod after, or
# modules.dep keeps naming files that are gone.
#
# kernel/net and kernel/fs stay whole: overlayfs, bridge, veth and netfilter are
# Docker's, not the hardware's, and virtiofs is a filesystem rather than a
# driver class — it lives in kernel/fs/fuse and is loaded on demand by the
# kernel when /etc/fstab asks for the virtiofs type.
#
# On arm64 the modules package also carries a device tree for every board the
# kernel boots on bare metal (about 100 MiB); a hypervisor describes this
# guest's hardware itself, so they go too.
RUN set -eux; \
    modules=$(ls -d /usr/lib/modules/*); \
    for class in \
        drivers/ata drivers/atm drivers/bcma drivers/bluetooth drivers/firewire \
        drivers/gnss drivers/gpu drivers/hid drivers/hwmon drivers/i2c \
        drivers/iio drivers/infiniband drivers/input drivers/isdn drivers/leds \
        drivers/media drivers/memstick drivers/mmc drivers/mtd \
        drivers/net/ethernet drivers/net/wireless drivers/nvme drivers/parport \
        drivers/pcmcia drivers/platform drivers/scsi drivers/ssb drivers/staging \
        drivers/thunderbolt drivers/usb drivers/w1 sound; \
    do \
        rm -rf "$modules/kernel/$class"; \
    done; \
    depmod -a "$(basename "$modules")"; \
    rm -rf "/usr/lib/firmware/$(basename "$modules")/device-tree"

# Ubuntu's image carries a login account, `ubuntu` (uid 1000, in sudo). Nobody
# logs in to the guest: it runs no getty and no ssh, and everything else runs
# in containers. An account with sudo has no business in it, so it goes.
RUN userdel --remove ubuntu

# udev's hardware database is a PCI, USB and OUI vendor table for naming devices
# that are not on this machine.
RUN rm -rf /usr/lib/udev/hwdb.bin /usr/lib/udev/hwdb.d

# containerd.io ships containerd's CLI. Docker drives containerd over its socket
# and starts the v2 shim; nothing here runs ctr.
RUN rm -f /usr/bin/ctr

# The root filesystem is mounted read-only and every virtio driver the guest
# needs is a module, so the initrd is what brings /dev/vda up. Build it
# explicitly rather than trusting the kernel package's postinst to have run to
# completion inside a container.
#
# The initrd's whole job is to mount an ext4 root on a virtio-blk disk, so it is
# built from a list rather than from MODULES=most, which packs every block and
# network driver Ubuntu ships. Names that are built into the kernel on an
# architecture are ignored there, so listing them costs nothing and covers the
# case where a flavour builds them as modules.
RUN set -eux; \
    printf '%s\n' ext4 virtio_blk virtio_console virtio_mmio virtio_pci \
        > /etc/initramfs-tools/modules; \
    sed -i 's/^MODULES=.*/MODULES=list/; s/^COMPRESS=.*/COMPRESS=zstd/' \
        /etc/initramfs-tools/initramfs.conf; \
    grep -q '^MODULES=list$' /etc/initramfs-tools/initramfs.conf; \
    grep -q '^COMPRESS=zstd$' /etc/initramfs-tools/initramfs.conf; \
    command -v zstd; \
    if ls /boot/initrd.img-* >/dev/null 2>&1; then \
        update-initramfs -u -k all; \
    else \
        update-initramfs -c -k all; \
    fi

# systemd-logind is masked, not merely unused: a headless guest has no seats and
# no PAM sessions, so it fails and is restarted in a loop that floods the serial
# console — the one diagnostic available for a guest whose Docker never answers.
# Its varlink socket goes with it: systemd starts that socket on its own,
# and with the service masked it fails to listen, a red line on every boot.
#
# systemd-networkd-persistent-storage is masked for the same reason and a
# simpler one: it saves lease state under /var/lib/systemd, the root is
# read-only, and there is nothing worth persisting for a DHCP lease a pool VM
# takes fresh on every boot. Unmasked it is a red FAILED line on every boot of
# every backend.
RUN systemctl mask \
        console-getty.service \
        dev-hugepages.mount \
        getty.target \
        systemd-logind.service \
        systemd-logind-varlink.socket \
        systemd-firstboot.service \
        systemd-networkd-persistent-storage.service \
        systemd-random-seed.service \
        systemd-remount-fs.service \
        sys-fs-fuse-connections.mount \
    && : > /etc/machine-id

COPY --from=guest-tools /out/discobox-vsock-guest /usr/local/bin/discobox-vsock-guest
COPY vm-image/discobox-storage.service /etc/systemd/system/discobox-storage.service
COPY --chmod=0755 vm-image/discobox-mount-storage /usr/local/sbin/discobox-mount-storage
COPY vm-image/discobox-remove-classic-store.service /etc/systemd/system/discobox-remove-classic-store.service
COPY vm-image/discobox-vsock-docker.service /etc/systemd/system/discobox-vsock-docker.service
COPY vm-image/discobox-vsock-lifecycle.service /etc/systemd/system/discobox-vsock-lifecycle.service
COPY --chmod=0755 vm-image/discobox-sync-clock /usr/local/sbin/discobox-sync-clock
COPY vm-image/discobox-timesync.service /etc/systemd/system/discobox-timesync.service
COPY vm-image/discobox-timesync.timer /etc/systemd/system/discobox-timesync.timer
COPY vm-image/docker.service.d/10-discobox.conf /etc/systemd/system/docker.service.d/10-discobox.conf
COPY vm-image/docker-daemon.json /etc/docker/daemon.json
COPY vm-image/systemd-networkd-wait-online.service.d/10-discobox.conf /etc/systemd/system/systemd-networkd-wait-online.service.d/10-discobox.conf
COPY vm-image/10-discobox.network /etc/systemd/network/10-discobox.network
COPY vm-image/fstab /etc/fstab

# /Users is the mount point for the host's read-only virtiofs share (see
# vm-image/fstab). It has to exist in the read-only root: the guest cannot
# create it later, and the pool-agent container binds paths under it.
RUN mkdir -p \
        /Users \
        /var/lib/discobox \
        /var/lib/discobox/cache \
        /var/lib/containerd \
        /var/lib/docker \
        /var/log \
        /var/tmp \
    && chmod 0711 /var/lib/containerd /var/lib/docker \
    && systemctl enable \
        discobox-storage.service \
        discobox-remove-classic-store.service \
        discobox-vsock-docker.service \
        discobox-vsock-lifecycle.service \
        discobox-timesync.timer \
        docker.service \
        docker.socket \
        systemd-networkd.service \
        systemd-networkd-wait-online.service \
        systemd-resolved.service

FROM ubuntu AS assemble
ARG TARGETARCH
ARG ROOT_SLACK_MIB=32

RUN cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends e2fsprogs gzip lz4 xz-utils zstd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=rootfs / /rootfs
COPY --chmod=0755 vm-image/assemble-guest-image /usr/local/sbin/assemble-guest-image
RUN TARGETARCH="$TARGETARCH" ROOT_SLACK_MIB="$ROOT_SLACK_MIB" assemble-guest-image /rootfs /out

FROM scratch
COPY --from=assemble /out/ /
