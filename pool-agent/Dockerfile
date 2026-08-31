# syntax=docker/dockerfile:1.7

# Declared before the first FROM so it is a global build argument the runtime
# stage's FROM can expand; declared alongside that FROM it would belong to the
# builder stage and resolve to nothing.
ARG BASE_IMAGE=discobox-base:local

# Build natively on the host platform and cross-compile, so multi-arch image
# builds only emulate the (apt-based) runtime stage, not the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie AS build
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY agentcreds ./agentcreds
COPY api ./api
COPY controlplane ./controlplane
COPY harness ./harness
COPY hostscope ./hostscope
COPY layout ./layout
COPY proxy ./proxy
COPY runcca ./runcca
COPY sandboxconfig ./sandboxconfig
COPY sandboxuser ./sandboxuser
COPY secretformat ./secretformat
COPY pool-agent/go.mod pool-agent/go.sum ./pool-agent/
COPY pool-agent/cmd ./pool-agent/cmd
COPY pool-agent ./pool-agent
WORKDIR /src/pool-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-agent ./cmd/discobox-pool-agent \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-runc ./cmd/discobox-pool-runc

# Debian, Docker, and the trimmed systemd unit set come from the shared base
# image so this image and the sandbox image are the same blobs on a pool host
# that pulls both. See base-image/Dockerfile.
FROM ${BASE_IMAGE}

ENV DISCOBOX_AGENT_PORT=3002

COPY --from=build /out/discobox-pool-agent /usr/local/bin/discobox-pool-agent
COPY pool-agent/image/systemd/discobox-proxy.service /etc/systemd/system/discobox-proxy.service

# The pool-shared builder and its output registry (ADR 0044). Both are taken
# from their upstream images rather than fetched as release tarballs, so the
# binary set stays exactly what those projects ship and test together.
#
# BuildKit's CNI plugins are the `buildkit-cni-*` binaries in /usr/bin, not a
# /opt/cni/bin tree: `--oci-worker-net=bridge` generates its own CNI config
# against them, which is why no cni.json is needed here.
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkitd /usr/local/bin/buildkitd
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildctl /usr/local/bin/buildctl
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-runc /usr/bin/buildkit-runc
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-bridge /usr/bin/buildkit-cni-bridge
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-firewall /usr/bin/buildkit-cni-firewall
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-host-local /usr/bin/buildkit-cni-host-local
COPY --from=moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-loopback /usr/bin/buildkit-cni-loopback
COPY --from=registry:2.8.3 /bin/registry /usr/local/bin/registry

COPY pool-agent/image/systemd/discobox-buildkitd.service /etc/systemd/system/discobox-buildkitd.service
COPY pool-agent/image/systemd/discobox-registry.service /etc/systemd/system/discobox-registry.service
COPY pool-agent/image/systemd/discobox-buildkit-mediator.service /etc/systemd/system/discobox-buildkit-mediator.service
COPY pool-agent/image/registry.yml /etc/discobox/registry.yml

# Installs as `runc` on the directory prepended to buildkitd's PATH, and is what
# --oci-worker-binary points at. It seeds the pool's MITM CA into every build
# step, so a user's Dockerfile needs no awareness of the proxy (ADR 0020/0044).
COPY --from=build /out/discobox-pool-runc /opt/discobox/bin/runc

# The pool agent runs dockerd outright, where the sandbox socket-activates it;
# that is why the base image enables neither.
RUN mkdir -p /var/lib/docker /var/lib/discobox /var/lib/discobox/proxy /etc/discobox /opt/discobox/bin \
    && systemctl enable docker.service discobox-proxy.service \
        discobox-buildkitd.service discobox-registry.service \
        discobox-buildkit-mediator.service

EXPOSE 3002
VOLUME ["/var/lib/docker", "/var/lib/discobox"]
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=6 CMD curl -fsS "http://127.0.0.1:${DISCOBOX_AGENT_PORT:-3002}/healthz" >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/discobox-pool-agent"]
