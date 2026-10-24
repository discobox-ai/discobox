# Declared before the first FROM so it is a global build argument the runtime
# stage's FROM can expand; declared alongside that FROM it would belong to the
# builder stage and resolve to nothing.
ARG BASE_IMAGE=discobox-sandbox-base:local

# Build natively on the host platform and cross-compile, so multi-arch image
# builds only emulate the (apt-based) runtime stage, not the Go compiler.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.27-trixie AS build
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY pkg/agentlayer ./pkg/agentlayer
COPY pkg/agentcreds ./pkg/agentcreds
COPY api ./api
COPY pkg/auditid ./pkg/auditid
COPY pkg/controlplane ./pkg/controlplane
COPY pkg/fsdurable ./pkg/fsdurable
COPY pkg/gitbackend ./pkg/gitbackend
COPY harness ./harness
COPY pkg/hostscope ./pkg/hostscope
COPY pkg/imagecache ./pkg/imagecache
COPY internal/filelock ./internal/filelock
COPY pkg/judge ./pkg/judge
COPY pkg/layout ./pkg/layout
COPY pkg/platform ./pkg/platform
COPY pkg/policy ./pkg/policy
COPY pkg/poolassets ./pkg/poolassets
COPY pkg/poolauth ./pkg/poolauth
COPY proxy ./proxy
COPY pkg/runcca ./pkg/runcca
COPY pkg/sandboxconfig ./pkg/sandboxconfig
COPY pkg/sandboxpath ./pkg/sandboxpath
COPY pkg/sandboxtree ./pkg/sandboxtree
COPY pkg/sandboxuser ./pkg/sandboxuser
COPY pkg/secretformat ./pkg/secretformat
COPY pkg/secretroute ./pkg/secretroute
COPY pkg/sourcecache ./pkg/sourcecache
COPY pkg/sourceref ./pkg/sourceref
COPY pkg/tarsums ./pkg/tarsums
COPY pkg/vsock ./pkg/vsock
COPY pkg/wellknown ./pkg/wellknown
COPY pkg/wire ./pkg/wire
COPY pkg/wstunnel ./pkg/wstunnel
COPY pool-agent/go.mod pool-agent/go.sum ./pool-agent/
COPY pool-agent/cmd ./pool-agent/cmd
COPY pool-agent ./pool-agent
WORKDIR /src/pool-agent
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-agent ./cmd/discobox-pool-agent \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/discobox-pool-runc ./cmd/discobox-pool-runc

# The pool bundle (ADR 26-10-24-953 §2): the features that make a box a
# pool's, which `discobox-provision apply` applies. `pool-agent` lays and
# starts the agent, which is also the pool proxy, and the proxy; `pool-builder`
# the pool-shared builder (ADR 0044), which a pool whose sandboxes are machines
# turns off. The same binaries the image below copies, each COPY --link, so an
# edit to one feature moves its layer and no other. BuildKit's and the
# registry's are taken from their upstream images, so the set stays what those
# projects ship and test together; BuildKit's CNI plugins are its
# `buildkit-cni-*` binaries, which `--oci-worker-net=bridge` configures itself.
# Built by name (`--target bundle`); the image stays the default target.
FROM scratch AS bundle
COPY --link pool-agent/bundle/features/pool-agent /features/pool-agent
COPY --link --from=build --chmod=0755 /out/discobox-pool-agent /features/pool-agent/discobox-pool-agent
COPY --link pool-agent/bundle/features/pool-builder /features/pool-builder
COPY --link --from=build --chmod=0755 /out/discobox-pool-runc /features/pool-builder/bin/discobox-pool-runc
COPY --link --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkitd /usr/bin/buildctl /usr/bin/buildkit-runc /usr/bin/buildkit-cni-bridge /usr/bin/buildkit-cni-firewall /usr/bin/buildkit-cni-host-local /usr/bin/buildkit-cni-loopback /features/pool-builder/bin/
COPY --link --from=public.ecr.aws/docker/library/registry:2.8.3 /bin/registry /features/pool-builder/bin/registry

# Debian, Docker, and the trimmed systemd unit set come from the Docker
# driver's sandbox base image, so this image and the sandboxes are the same
# blobs on a pool host that pulls both. See
# server/providers/dockerworker/sandbox-image/Dockerfile.
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
COPY --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkitd /usr/local/bin/buildkitd
COPY --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildctl /usr/local/bin/buildctl
COPY --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkit-runc /usr/bin/buildkit-runc
COPY --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-bridge /usr/bin/buildkit-cni-bridge
COPY --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-firewall /usr/bin/buildkit-cni-firewall
COPY --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-host-local /usr/bin/buildkit-cni-host-local
COPY --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkit-cni-loopback /usr/bin/buildkit-cni-loopback
COPY --from=public.ecr.aws/docker/library/registry:2.8.3 /bin/registry /usr/local/bin/registry

COPY pool-agent/image/systemd/discobox-buildkitd.service /etc/systemd/system/discobox-buildkitd.service
COPY pool-agent/image/systemd/discobox-registry.service /etc/systemd/system/discobox-registry.service
COPY pool-agent/image/systemd/discobox-buildkit-mediator.service /etc/systemd/system/discobox-buildkit-mediator.service
COPY pool-agent/image/registry.yml /etc/discobox/registry.yml

# Installs as `runc` on the directory prepended to buildkitd's PATH, and is what
# --oci-worker-binary points at. It seeds the pool's MITM CA into every build
# step, so a user's Dockerfile needs no awareness of the proxy (ADR 0020/0044).
COPY --from=build /out/discobox-pool-runc /opt/discobox/bin/runc

# The pool agent runs dockerd outright, where the base image socket-activates
# it for a sandbox.
RUN mkdir -p /var/lib/docker /var/lib/discobox /var/lib/discobox/proxy /etc/discobox /opt/discobox/bin \
    && systemctl enable docker.service discobox-proxy.service \
        discobox-buildkitd.service discobox-registry.service \
        discobox-buildkit-mediator.service

EXPOSE 3002
VOLUME ["/var/lib/docker", "/var/lib/discobox"]
HEALTHCHECK --interval=5s --timeout=2s --start-period=5s --retries=6 CMD curl -fsS "http://127.0.0.1:${DISCOBOX_AGENT_PORT:-3002}/healthz" >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/discobox-pool-agent"]
