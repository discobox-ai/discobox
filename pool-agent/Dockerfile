# Build natively on the host platform and cross-compile: the bundle is
# FROM scratch, so a multi-arch build runs nothing under emulation.
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
COPY pkg/gitprogress ./pkg/gitprogress
COPY harness ./harness
COPY pkg/harnesscapture ./pkg/harnesscapture
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
# turns off. Each COPY --link, so an edit to one feature moves its layer and
# no other. BuildKit's and the registry's are taken from their upstream
# images, so the set stays what those projects ship and test together;
# BuildKit's CNI plugins are its `buildkit-cni-*` binaries, which
# `--oci-worker-net=bridge` configures itself.
# A pool's container runs the sandbox base image, so there is no pool image:
# this is the Dockerfile's one product, and its default target.
FROM scratch AS bundle
COPY --link pool-agent/bundle/features/pool-agent /features/pool-agent
COPY --link --from=build --chmod=0755 /out/discobox-pool-agent /features/pool-agent/discobox-pool-agent
COPY --link pool-agent/bundle/features/pool-builder /features/pool-builder
COPY --link --from=build --chmod=0755 /out/discobox-pool-runc /features/pool-builder/bin/discobox-pool-runc
COPY --link --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkitd /usr/bin/buildctl /usr/bin/buildkit-runc /usr/bin/buildkit-cni-bridge /usr/bin/buildkit-cni-firewall /usr/bin/buildkit-cni-host-local /usr/bin/buildkit-cni-loopback /features/pool-builder/bin/
COPY --link --from=public.ecr.aws/docker/library/registry:2.8.3 /bin/registry /features/pool-builder/bin/registry
