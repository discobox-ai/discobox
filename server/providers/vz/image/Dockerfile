# syntax=docker/dockerfile:1.7

# The Discobox macOS guest image (ADR 0062).
#
# The guest is Docker's host and nothing else: everything Discobox does runs in
# containers on the daemon this image boots. Nothing that is not needed to bring
# dockerd and the two VSOCK helpers up belongs here.
#
# The build produces the complete boot artifact set in its final stage —
# an uncompressed kernel, an initrd, and a raw ext4 root filesystem — so the
# same Dockerfile is buildable by CI, by a Linux developer, and by BuildKit
# inside a running pool VM (ADR 0062 §6, §7). Filesystem assembly runs in a
# RUN step: mkfs.ext4 -d populates an image file with no loop device and no
# privileges, so no host-side fakeroot or qemu-img step exists to be missing on
# a Mac.

FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS guest-tools
ARG TARGETOS=linux
ARG TARGETARCH=arm64

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
ARG TARGETARCH=arm64

ENV container=discobox-vz

# linux-image-<arch> is the only reason this image is not tiny, and it is what
# removes a kernel build from the project: Virtualization.framework needs no
# patched kernel, unlike libkrun (ADR 0062 §8).
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

# The root filesystem is mounted read-only and every virtio driver the guest
# needs is a module, so the initrd is what brings /dev/vda up. Build it
# explicitly rather than trusting the kernel package's postinst to have run to
# completion inside a container.
RUN set -eux; \
    if ls /boot/initrd.img-* >/dev/null 2>&1; then \
        update-initramfs -u -k all; \
    else \
        update-initramfs -c -k all; \
    fi

# systemd-logind is masked, not merely unused: a headless guest has no seats and
# no PAM sessions, so it fails and is restarted in a loop that floods the serial
# console — the one diagnostic available for a guest whose Docker never answers.
RUN systemctl mask \
        console-getty.service \
        dev-hugepages.mount \
        getty.target \
        systemd-logind.service \
        systemd-firstboot.service \
        systemd-networkd-wait-online.service \
        systemd-random-seed.service \
        systemd-remount-fs.service \
        sys-fs-fuse-connections.mount \
    && : > /etc/machine-id

COPY --from=guest-tools /out/discobox-vsock-guest /usr/local/bin/discobox-vsock-guest
COPY server/providers/vz/image/discobox-storage.service /etc/systemd/system/discobox-storage.service
COPY --chmod=0755 server/providers/vz/image/discobox-mount-storage /usr/local/sbin/discobox-mount-storage
COPY server/providers/vz/image/discobox-vsock-docker.service /etc/systemd/system/discobox-vsock-docker.service
COPY server/providers/vz/image/discobox-vsock-lifecycle.service /etc/systemd/system/discobox-vsock-lifecycle.service
COPY server/providers/vz/image/docker.service.d/10-discobox.conf /etc/systemd/system/docker.service.d/10-discobox.conf
COPY server/providers/vz/image/10-discobox.network /etc/systemd/network/10-discobox.network
COPY server/providers/vz/image/discobox-vsock.conf /etc/modules-load.d/discobox-vsock.conf
COPY server/providers/vz/image/fstab /etc/fstab

RUN mkdir -p \
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
        docker.service \
        docker.socket \
        systemd-networkd.service \
        systemd-resolved.service

FROM debian:13-slim AS assemble
ARG TARGETARCH=arm64
ARG ROOT_SLACK_MIB=192

RUN apt-get update \
    && apt-get install -y --no-install-recommends e2fsprogs gzip lz4 xz-utils zstd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

COPY --from=rootfs / /rootfs
COPY --chmod=0755 server/providers/vz/image/assemble-guest-image /usr/local/sbin/assemble-guest-image
RUN TARGETARCH="$TARGETARCH" ROOT_SLACK_MIB="$ROOT_SLACK_MIB" assemble-guest-image /rootfs /out

FROM scratch
COPY --from=assemble /out/ /
