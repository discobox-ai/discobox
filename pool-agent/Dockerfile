# Go is upstream's release, on the repository's one Go pin, and the stage is
# Ubuntu, on its one Ubuntu pin (see "Ubuntu" in the root DESIGN.md): the
# LTS's Go is older than go.mod asks for, and the golang images are Debian.
# Built natively on the host platform and cross-compiled: the bundle is FROM
# scratch, so a multi-arch build runs nothing under emulation.
ARG UBUNTU_IMAGE=public.ecr.aws/docker/library/ubuntu:resolute-20261105.1@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db
ARG UBUNTU_MIRROR_AMD64=http://mirrors.edge.kernel.org/ubuntu
ARG UBUNTU_MIRROR_ARM64=http://mirrors.mit.edu/ubuntu-ports
ARG GO_VERSION=1.27.3
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49

FROM --platform=$BUILDPLATFORM ${UBUNTU_IMAGE} AS go
ARG UBUNTU_MIRROR_AMD64
ARG UBUNTU_MIRROR_ARM64
ARG GO_VERSION
ARG GO_SHA256_AMD64
ARG GO_SHA256_ARM64
RUN set -eu \
    && case "$(dpkg --print-architecture)" in \
        amd64) first=$UBUNTU_MIRROR_AMD64 second=http://ftp.osuosl.org/pub/ubuntu third=http://azure.archive.ubuntu.com/ubuntu ;; \
        arm64) first=$UBUNTU_MIRROR_ARM64 second=http://azure.ports.ubuntu.com/ubuntu-ports third=http://ports.ubuntu.com/ubuntu-ports ;; \
        *) echo "no Ubuntu mirror for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && mkdir -p /etc/apt/mirrors \
    && printf '%s\tpriority:1\n%s\tpriority:2\n%s\tpriority:3\n' "$first" "$second" "$third" > /etc/apt/mirrors/ubuntu.list \
    && printf '%s\n' 'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' '' \
        'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute-updates resolute-security' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' 'Valid-Until-Max: 604800' \
        > /etc/apt/sources.list.d/ubuntu.sources \
    && printf '%s\n' 'Acquire::Retries "5";' 'Acquire::http::Timeout "5";' > /etc/apt/apt.conf.d/80-discobox-apt \
    && echo "apt reads $first, then $second, then $third"
RUN cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
RUN set -eu \
    && arch="$(dpkg --print-architecture)" \
    && case "$arch" in \
        amd64) go_sha=$GO_SHA256_AMD64 ;; \
        arm64) go_sha=$GO_SHA256_ARM64 ;; \
        *) echo "no Go for $arch" >&2; exit 1 ;; \
    esac \
    && curl -fsSL --retry 5 --retry-all-errors -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" \
    && echo "$go_sha  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz \
    && /usr/local/go/bin/go version
# The pinned Go is the Go that builds: a go.mod that asks for a newer one
# fails here, rather than fetching a toolchain nothing pins. Moving it is
# `go tool task go:bump`.
ENV PATH=/usr/local/go/bin:${PATH} GOPATH=/go GOTOOLCHAIN=local

FROM go AS build
ARG TARGETOS TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
COPY pkg/agentlayer ./pkg/agentlayer
COPY pkg/agentcreds ./pkg/agentcreds
COPY api ./api
COPY pkg/auditid ./pkg/auditid
COPY pkg/controlplane ./pkg/controlplane
COPY pkg/execstream ./pkg/execstream
COPY pkg/fsdurable ./pkg/fsdurable
COPY pkg/genfile ./pkg/genfile
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
COPY pkg/provisionspec ./pkg/provisionspec
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
