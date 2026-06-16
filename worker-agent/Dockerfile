# syntax=docker/dockerfile:1

FROM golang:1.26-trixie AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY id ./id
COPY model ./model
COPY sandboxprovider ./sandboxprovider
COPY workerbootstrap ./workerbootstrap
COPY worker-agent/go.mod worker-agent/go.sum ./worker-agent/
COPY worker-agent/cmd ./worker-agent/cmd
COPY worker-agent/workeragent ./worker-agent/workeragent
WORKDIR /src/worker-agent
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/discobox-worker-agent ./cmd/discobox-worker-agent

FROM debian:13-slim

ENV container=docker
ENV DISCOBOX_AGENT_PORT=3002

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        dbus \
        docker.io \
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
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/discobox-worker-agent"]
