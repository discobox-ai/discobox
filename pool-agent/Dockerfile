# Go is Debian sid's own, from the repository's one Debian pin (see "Debian"
# in the root DESIGN.md): there is no golang image on sid. Built natively on
# the host platform and cross-compiled: the bundle is FROM scratch, so a
# multi-arch build runs nothing under emulation.
ARG DEBIAN_IMAGE=public.ecr.aws/docker/library/debian:sid-20261104-slim@sha256:07a3205e8583fdff12d2b05bd030f50437e9fd340ee616df0c05dc77af75767b
ARG DEBIAN_SNAPSHOT=20261111T202414Z

FROM --platform=$BUILDPLATFORM ${DEBIAN_IMAGE} AS go
ARG DEBIAN_SNAPSHOT
RUN printf '%s\n' 'Types: deb' "URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}" \
        'Suites: sid' 'Components: main' 'Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp' \
        'Check-Valid-Until: no' > /etc/apt/sources.list.d/debian.sources \
    && echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-discobox-retries \
    && s=$(date -u -d "$(echo "$DEBIAN_SNAPSHOT" | sed -E 's/^(.{4})(..)(..)T(..)(..)(..)Z$/\1-\2-\3 \4:\5:\6 UTC/')" +%s) \
    && printf '%s\n' '#!/bin/sh' \
        'set -eu' \
        "released=\$(sed -n 's/^Date: //p' /var/lib/apt/lists/*_dists_sid_InRelease /var/lib/apt/lists/*_dists_sid_Release 2>/dev/null | head -n 1)" \
        "[ -n \"\$released\" ] || { echo 'apt fetched no signed sid Release to check against snapshot ${DEBIAN_SNAPSHOT}: refusing it' >&2; exit 1; }" \
        "d=\$(date -u -d \"\$released\" +%s)" \
        "[ \"\$d\" -le $s ] && [ \"\$d\" -gt $((s - 86400)) ] && exit 0" \
        "echo \"the sid index apt fetched is dated \$released, not snapshot ${DEBIAN_SNAPSHOT}'s: refusing it\" >&2" \
        'exit 1' > /usr/local/sbin/discobox-apt-snapshot \
    && chmod 0755 /usr/local/sbin/discobox-apt-snapshot \
    && echo 'APT::Update::Post-Invoke-Success { "/usr/local/sbin/discobox-apt-snapshot"; };' > /etc/apt/apt.conf.d/81-discobox-snapshot \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates golang-go \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
# The snapshot's Go is the Go that builds: a go.mod that asks for a newer one
# fails here, rather than fetching a toolchain nothing pins (Debian's Go
# defaults to GOTOOLCHAIN=auto). Moving to it is a bump of the Debian pin.
ENV GOPATH=/go GOTOOLCHAIN=local

FROM go AS build
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
COPY pkg/layout ./pkg/layout
COPY pkg/platform ./pkg/platform
COPY pkg/poolassets ./pkg/poolassets
COPY pkg/poolauth ./pkg/poolauth
COPY pkg/pooloutbound ./pkg/pooloutbound
COPY proxy ./proxy
COPY pkg/runcca ./pkg/runcca
COPY pkg/sandboxconfig ./pkg/sandboxconfig
COPY pkg/sandboxpath ./pkg/sandboxpath
COPY pkg/sandboxtree ./pkg/sandboxtree
COPY pkg/sandboxuser ./pkg/sandboxuser
COPY pkg/secretroute ./pkg/secretroute
COPY pkg/sourcecache ./pkg/sourcecache
COPY pkg/sourceref ./pkg/sourceref
COPY pkg/tarsums ./pkg/tarsums
COPY pkg/vsock ./pkg/vsock
COPY pkg/wire ./pkg/wire
COPY pkg/wstunnel ./pkg/wstunnel
COPY pkg/wsrecord ./pkg/wsrecord
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
# the pool-shared builder (ADR 0044). `pool-builder-units` installs the builder's
# dependency graph before an agent restart, including on an existing pool. Each COPY --link, so an edit to one feature moves its layer and
# no other. BuildKit's and the registry's are taken from their upstream
# images, so the set stays what those projects ship and test together;
# BuildKit's CNI plugins are its `buildkit-cni-*` binaries, which
# `--oci-worker-net=bridge` configures itself.
# A pool's container runs the sandbox base image, so there is no pool image:
# this is the Dockerfile's one product, and its default target.
FROM scratch AS bundle
COPY --link pool-agent/bundle/features/pool-builder-units /features/pool-builder-units
COPY --link pool-agent/bundle/features/pool-agent /features/pool-agent
COPY --link --from=build --chmod=0755 /out/discobox-pool-agent /features/pool-agent/discobox-pool-agent
COPY --link pool-agent/bundle/features/pool-builder /features/pool-builder
COPY --link --from=build --chmod=0755 /out/discobox-pool-runc /features/pool-builder/bin/discobox-pool-runc
COPY --link --from=mirror.gcr.io/moby/buildkit:v0.32.2 /usr/bin/buildkitd /usr/bin/buildctl /usr/bin/buildkit-runc /usr/bin/buildkit-cni-bridge /usr/bin/buildkit-cni-firewall /usr/bin/buildkit-cni-host-local /usr/bin/buildkit-cni-loopback /features/pool-builder/bin/
COPY --link --from=public.ecr.aws/docker/library/registry:2.8.3 /bin/registry /features/pool-builder/bin/registry
