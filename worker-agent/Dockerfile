# syntax=docker/dockerfile:1.7

FROM golang:1.26-trixie AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY controlplane ./controlplane
COPY id ./id
COPY worker-agent/go.mod worker-agent/go.sum ./worker-agent/
COPY worker-agent/cmd ./worker-agent/cmd
COPY worker-agent ./worker-agent
WORKDIR /src/worker-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/discobox-worker-agent ./cmd/discobox-worker-agent

FROM debian:13-slim

ENV container=docker
ENV DISCOBOX_AGENT_PORT=3002

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        dbus \
        docker.io \
        git \
        systemd \
        systemd-sysv \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl mask \
        dev-hugepages.mount \
        sys-fs-fuse-connections.mount \
        systemd-remount-fs.service \
        getty.target \
        console-getty.service

COPY --from=build /out/discobox-worker-agent /usr/local/bin/discobox-worker-agent

RUN mkdir -p /var/lib/docker /var/lib/discobox \
    && systemctl enable docker.service

EXPOSE 3002
VOLUME ["/var/lib/docker", "/var/lib/discobox"]
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=6 CMD curl -fsS "http://127.0.0.1:${DISCOBOX_AGENT_PORT:-3002}/healthz" >/dev/null || exit 1
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/discobox-worker-agent"]
