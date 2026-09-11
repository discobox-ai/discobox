# syntax=docker/dockerfile:1.7

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

FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS guest-tools
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY pool-agent/go.mod pool-agent/go.sum ./pool-agent/
COPY pool-agent/vsock ./pool-agent/vsock
COPY pool-agent/cmd/discobox-vsock-guest ./pool-agent/cmd/discobox-vsock-guest
WORKDIR /src/pool-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/discobox-vsock-guest ./cmd/discobox-vsock-guest

FROM debian:13-slim AS rootfs
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

# linux-image-<arch> is the only reason this image is not tiny, and it is what
# removes a kernel build from the backends that can use it: a hypervisor taking
# stock virtio needs no patched kernel (ADR 0062 §8). It is installed on every
# architecture so this build has no per-backend branch in it, and a backend that
# brings its own kernel simply does not ask for the one assembly lifts out.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        dbus \
        docker.io \
        e2fsprogs \
        initramfs-tools \
        iproute2 \
        iputils-ping \
        linux-image-${TARGETARCH} \
        systemd \
        systemd-resolved \
        systemd-sysv \
        util-linux \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Debian's kernel package carries a module for every device Debian supports.
# This guest's entire hardware surface is virtio — block, net, vsock, console,
# entropy, balloon, fs — whichever hypervisor attaches it, so whole driver
# classes here describe hardware that cannot appear. depmod after, or
# modules.dep keeps naming files that are gone.
#
# kernel/net and kernel/fs stay whole: overlayfs, bridge, veth and netfilter are
# Docker's, not the hardware's, and virtiofs is a filesystem rather than a
# driver class — it lives in kernel/fs/fuse and is loaded on demand by the
# kernel when /etc/fstab asks for the virtiofs type.
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
    depmod -a "$(basename "$modules")"

# udev's hardware database is a PCI, USB and OUI vendor table for naming devices
# that are not on this machine.
RUN rm -rf /usr/lib/udev/hwdb.bin /usr/lib/udev/hwdb.d

# Debian's containerd package ships the CLI and both v1 shims. Docker drives
# containerd over its socket and starts the v2 shim; nothing here runs the rest.
RUN rm -f \
        /usr/bin/containerd-shim \
        /usr/bin/containerd-shim-runc-v1 \
        /usr/bin/containerd-stress \
        /usr/bin/ctr

# The root filesystem is mounted read-only and every virtio driver the guest
# needs is a module, so the initrd is what brings /dev/vda up. Build it
# explicitly rather than trusting the kernel package's postinst to have run to
# completion inside a container.
#
# The initrd's whole job is to mount an ext4 root on a virtio-blk disk, so it is
# built from a list rather than from MODULES=most, which packs every block and
# network driver Debian ships. Names that are built into the kernel on an
# architecture are ignored there, so listing them costs nothing and covers the
# case where a flavour builds them as modules.
RUN set -eux; \
    printf '%s\n' ext4 virtio_blk virtio_console virtio_mmio virtio_pci \
        > /etc/initramfs-tools/modules; \
    sed -i 's/^MODULES=.*/MODULES=list/; s/^COMPRESS=.*/COMPRESS=zstd/' \
        /etc/initramfs-tools/initramfs.conf; \
    grep -q '^MODULES=list$' /etc/initramfs-tools/initramfs.conf; \
    grep -q '^COMPRESS=zstd$' /etc/initramfs-tools/initramfs.conf; \
    if ls /boot/initrd.img-* >/dev/null 2>&1; then \
        update-initramfs -u -k all; \
    else \
        update-initramfs -c -k all; \
    fi

# systemd-logind is masked, not merely unused: a headless guest has no seats and
# no PAM sessions, so it fails and is restarted in a loop that floods the serial
# console — the one diagnostic available for a guest whose Docker never answers.
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
        systemd-firstboot.service \
        systemd-networkd-persistent-storage.service \
        systemd-random-seed.service \
        systemd-remount-fs.service \
        sys-fs-fuse-connections.mount \
    && : > /etc/machine-id

COPY --from=guest-tools /out/discobox-vsock-guest /usr/local/bin/discobox-vsock-guest
COPY vm-image/discobox-storage.service /etc/systemd/system/discobox-storage.service
COPY --chmod=0755 vm-image/discobox-mount-storage /usr/local/sbin/discobox-mount-storage
COPY vm-image/discobox-vsock-docker.service /etc/systemd/system/discobox-vsock-docker.service
COPY vm-image/discobox-vsock-lifecycle.service /etc/systemd/system/discobox-vsock-lifecycle.service
COPY --chmod=0755 vm-image/discobox-sync-clock /usr/local/sbin/discobox-sync-clock
COPY vm-image/discobox-timesync.service /etc/systemd/system/discobox-timesync.service
COPY vm-image/discobox-timesync.timer /etc/systemd/system/discobox-timesync.timer
COPY vm-image/docker.service.d/10-discobox.conf /etc/systemd/system/docker.service.d/10-discobox.conf
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
        discobox-vsock-docker.service \
        discobox-vsock-lifecycle.service \
        discobox-timesync.timer \
        docker.service \
        docker.socket \
        systemd-networkd.service \
        systemd-networkd-wait-online.service \
        systemd-resolved.service

FROM debian:13-slim AS assemble
ARG TARGETARCH
ARG ROOT_SLACK_MIB=32

RUN apt-get update \
    && apt-get install -y --no-install-recommends e2fsprogs gzip lz4 xz-utils zstd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=rootfs / /rootfs
COPY --chmod=0755 vm-image/assemble-guest-image /usr/local/sbin/assemble-guest-image
RUN TARGETARCH="$TARGETARCH" ROOT_SLACK_MIB="$ROOT_SLACK_MIB" assemble-guest-image /rootfs /out

FROM scratch
COPY --from=assemble /out/ /
