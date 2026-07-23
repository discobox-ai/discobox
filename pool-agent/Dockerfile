# syntax=docker/dockerfile:1.7

# Build natively on the host platform and cross-compile, so multi-arch image
# builds only emulate the (apt-based) runtime stage, not the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS build
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY api ./api
COPY controlplane ./controlplane
COPY harness ./harness
COPY id ./id
COPY proxy ./proxy
COPY sandboxconfig ./sandboxconfig
COPY gormdb ./gormdb
COPY pool-agent/go.mod pool-agent/go.sum ./pool-agent/
COPY pool-agent/cmd ./pool-agent/cmd
COPY pool-agent ./pool-agent
WORKDIR /src/pool-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-agent ./cmd/discobox-pool-agent

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

COPY --from=build /out/discobox-pool-agent /usr/local/bin/discobox-pool-agent
COPY pool-agent/image/systemd/discobox-proxy.service /etc/systemd/system/discobox-proxy.service

RUN mkdir -p /var/lib/docker /var/lib/discobox /var/lib/discobox/proxy /etc/discobox \
    && systemctl enable docker.service discobox-proxy.service

EXPOSE 3002
VOLUME ["/var/lib/docker", "/var/lib/discobox"]
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=6 CMD curl -fsS "http://127.0.0.1:${DISCOBOX_AGENT_PORT:-3002}/healthz" >/dev/null || exit 1
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/discobox-pool-agent"]
