# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS guest-tools
ARG TARGETOS=linux
ARG TARGETARCH=amd64

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

FROM debian:13-slim

ENV container=discobox-krun

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        dbus \
        docker.io \
        iproute2 \
        iputils-ping \
        systemd \
        systemd-sysv \
        util-linux \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN systemctl mask \
        console-getty.service \
        dev-hugepages.mount \
        getty.target \
        systemd-firstboot.service \
        systemd-networkd-wait-online.service \
        systemd-random-seed.service \
        systemd-remount-fs.service \
        sys-fs-fuse-connections.mount \
    && : > /etc/machine-id

COPY --from=guest-tools /out/discobox-vsock-guest /usr/local/bin/discobox-vsock-guest
COPY server/providers/libkrun/image/discobox-storage.service /etc/systemd/system/discobox-storage.service
COPY --chmod=0755 server/providers/libkrun/image/discobox-mount-storage /usr/local/sbin/discobox-mount-storage
COPY server/providers/libkrun/image/discobox-network.service /etc/systemd/system/discobox-network.service
COPY --chmod=0755 server/providers/libkrun/image/discobox-configure-network /usr/local/sbin/discobox-configure-network
COPY server/providers/libkrun/image/discobox-vsock-docker.service /etc/systemd/system/discobox-vsock-docker.service
COPY server/providers/libkrun/image/discobox-vsock-lifecycle.service /etc/systemd/system/discobox-vsock-lifecycle.service
COPY server/providers/libkrun/image/docker.service.d/10-discobox.conf /etc/systemd/system/docker.service.d/10-discobox.conf

RUN mkdir -p \
        /var/lib/discobox \
        /var/lib/discobox/cache \
        /var/lib/containerd \
        /var/lib/docker \
        /var/log \
        /var/tmp \
    && chmod 0711 /var/lib/containerd /var/lib/docker \
    && systemctl enable \
        discobox-network.service \
        discobox-storage.service \
        discobox-vsock-docker.service \
        discobox-vsock-lifecycle.service \
        docker.service \
        docker.socket \
    && systemctl disable systemd-networkd.service

COPY server/providers/libkrun/image/fstab /etc/fstab

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
