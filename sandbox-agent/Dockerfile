# Go is upstream's release, on the repository's one Go pin, and the stage is
# Ubuntu, on its one Ubuntu pin (see "Ubuntu" in the root DESIGN.md): the
# LTS's Go is older than go.mod asks for, and the golang images are Debian.
# Built natively on the host platform and cross-compiled, so the platform
# bundle's every architecture is built with nothing emulated.
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

FROM go AS sandbox-agent-builder
ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY pkg/agentcreds ./pkg/agentcreds
COPY pkg/agentlayer ./pkg/agentlayer
COPY access ./access
COPY api ./api
COPY pkg/auditid ./pkg/auditid
COPY pkg/copyarchive ./pkg/copyarchive
COPY pkg/declared ./pkg/declared
COPY pkg/execstream ./pkg/execstream
COPY pkg/fsdurable ./pkg/fsdurable
COPY pkg/gitbackend ./pkg/gitbackend
COPY pkg/gitprogress ./pkg/gitprogress
COPY harness ./harness
COPY pkg/hostscope ./pkg/hostscope
COPY pkg/harnesscapture ./pkg/harnesscapture
COPY pkg/imagecache ./pkg/imagecache
COPY internal/filelock ./internal/filelock
COPY pkg/platform ./pkg/platform
COPY pkg/poolassets ./pkg/poolassets
COPY pkg/poolauth ./pkg/poolauth
COPY pkg/pooloutbound ./pkg/pooloutbound
COPY pkg/programstatus ./pkg/programstatus
COPY pkg/provisionspec ./pkg/provisionspec
COPY proxy ./proxy
COPY pkg/runcca ./pkg/runcca
COPY pkg/sandboxconfig ./pkg/sandboxconfig
COPY pkg/sandboxpath ./pkg/sandboxpath
COPY pkg/sandboxmeta ./pkg/sandboxmeta
COPY pkg/sandboxservices ./pkg/sandboxservices
COPY pkg/sandboxshell ./pkg/sandboxshell
COPY pkg/sandboxtree ./pkg/sandboxtree
COPY pkg/sandboxuser ./pkg/sandboxuser
COPY pkg/secretroute ./pkg/secretroute
COPY pkg/sourcecache ./pkg/sourcecache
COPY pkg/sourceref ./pkg/sourceref
COPY pkg/sse ./pkg/sse
COPY pkg/tarsums ./pkg/tarsums
COPY pkg/tools ./pkg/tools
COPY pkg/vsock ./pkg/vsock
COPY pkg/wire ./pkg/wire
COPY pkg/wstunnel ./pkg/wstunnel
COPY sandbox-agent ./sandbox-agent

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-sandbox-agent ./cmd/discobox-sandbox-agent \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C access build -trimpath -ldflags="-s -w" -o /out/discobox-access ./cmd/discobox-access

# Built in its own layer so bumping the review version does not invalidate the
# sandbox-agent binaries above.
# Pinned to a main pseudo-version rather than `latest`: review carries no
# tags, so `latest` is main's tip anyway, but an unpinned version makes the
# build unreproducible and leaves this layer cached on a stale commit. Bump it
# to pull a newer main.
#
# Bumping it can also invalidate the `discobox-review` skill, which documents
# this binary's interface from another repository and is installed further
# down: re-read `sandbox-agent/features/core/skills/discobox-review/SKILL.md` against
# the version you move to. Nothing else in the build will notice.
ARG REVIEW_VERSION=v0.0.0-20261024044123-efdd16533e4e
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    set -eux \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
        go install -trimpath -ldflags="-s -w" github.com/discobox-ai/review/cmd/discobox-review@${REVIEW_VERSION} \
    # `go install` refuses a GOBIN with cross-compilation, so where the binary
    # lands depends on how this image is being built. depot builds each
    # architecture on a native machine and it is GOPATH/bin; the emulated
    # buildx fallback cross-compiles the foreign leg to keep the Go stage off
    # QEMU, and it is GOPATH/bin/<goos>_<goarch>. Both are reachable, so take
    # whichever was used.
    #
    # The two moves are grouped so that the fallback belongs to the first move
    # alone. Ungrouped, `install && mv A || mv B` is `(install && mv A) || mv
    # B`, so a failed install runs the fallback too and the build reports a
    # missing file instead of why the install failed.
    && { mv "$(go env GOPATH)/bin/${TARGETOS}_${TARGETARCH}/discobox-review" /out/discobox-review \
        || mv "$(go env GOPATH)/bin/discobox-review" /out/discobox-review; }

# The discobox CLI, which a discobox calls the discobox API with through its
# pool (ADR 0140). Its own stage: the CLI reaches much more of the root module
# than the sandbox agent does, and building it beside the agent would rebuild
# the agent whenever any of that changed. The directories are the root-module
# packages it imports (go list -deps ./cli/cmd/discobox), then its own module
# and termpane, which its go.mod replaces with the paths beside it.
#
# DISCOBOX_VERSION is the release the bundle is cut from, which the release
# build passes so that `discobox --version` in a box names it and a bug report
# filed from one can say which release it was. The build context has no .git,
# so without it the CLI reports "unknown"; a development build leaves it so.
FROM go AS discobox-cli-builder
ARG TARGETOS TARGETARCH
ARG DISCOBOX_VERSION=

WORKDIR /src

COPY go.mod go.sum ./
COPY pkg/agentcreds ./pkg/agentcreds
COPY api ./api
COPY pkg/auditid ./pkg/auditid
COPY pkg/copyarchive ./pkg/copyarchive
COPY pkg/bindingplan ./pkg/bindingplan
COPY pkg/configfile ./pkg/configfile
COPY pkg/controlplane ./pkg/controlplane
COPY pkg/credentialpolicy ./pkg/credentialpolicy
COPY pkg/declared ./pkg/declared
COPY pkg/endpoint ./pkg/endpoint
COPY pkg/execstream ./pkg/execstream
COPY pkg/fsdurable ./pkg/fsdurable
COPY pkg/genfile ./pkg/genfile
COPY harness ./harness
COPY pkg/health ./pkg/health
COPY pkg/hostscope ./pkg/hostscope
COPY pkg/harnesscapture ./pkg/harnesscapture
COPY pkg/imagecache ./pkg/imagecache
COPY internal ./internal
COPY pkg/platform ./pkg/platform
COPY pkg/programstatus ./pkg/programstatus
COPY pkg/provisionspec ./pkg/provisionspec
COPY pkg/randomname ./pkg/randomname
COPY pkg/releasemanifest ./pkg/releasemanifest
COPY pkg/sandboxconfig ./pkg/sandboxconfig
COPY pkg/sandboxpath ./pkg/sandboxpath
COPY pkg/sandboxmeta ./pkg/sandboxmeta
COPY pkg/sandboxservices ./pkg/sandboxservices
COPY pkg/sandboxshell ./pkg/sandboxshell
COPY pkg/sandboxuser ./pkg/sandboxuser
COPY pkg/secretformat ./pkg/secretformat
COPY pkg/secretroute ./pkg/secretroute
COPY pkg/serverstage ./pkg/serverstage
COPY pkg/sourcecache ./pkg/sourcecache
COPY pkg/sourceref ./pkg/sourceref
COPY pkg/requestprogress ./pkg/requestprogress
COPY pkg/sse ./pkg/sse
COPY pkg/tarsums ./pkg/tarsums
COPY pkg/tools ./pkg/tools
COPY pkg/version ./pkg/version
COPY pkg/wsrecord ./pkg/wsrecord
COPY termpane ./termpane
COPY cli ./cli

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C cli build -trimpath -ldflags="-s -w${DISCOBOX_VERSION:+ -X github.com/discobox-ai/discobox/pkg/version.Version=$DISCOBOX_VERSION}" -o /out/discobox ./cmd/discobox

# The agent bundle (ADR 26-10-24-953 §2): the agent alone, one layer, which
# whoever makes a box places in it before the box applies its bundles — a pool
# in a sandbox, a provider in a pool — and is not in any box's list. FROM
# scratch, and one COPY, so the bundle is that one layer
# (agentlayer.ExtractAgent). Built by name (`--target agent`).
FROM scratch AS agent
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-sandbox-agent /features/core/discobox-sandbox-agent
# The sandbox.json apiVersion values this agent reads, which the pool reads to
# choose the one it writes (ADR 26-11-24-519 §1): config, not a layer, so the
# bundle stays the agent alone. sandboxconfig.ReadAPIVersions, held equal by
# sandbox-agent/config's TestTheAgentBundleDeclaresWhatTheAgentReads.
LABEL io.discobox.agent.v1.sandbox-config='["discobox.ai/sandbox/v1","discobox.dev/sandbox/v1"]'

# The platform bundle (ADR 26-10-16-423 §7), each COPY --link: a box fetches
# only the layers its store lacks, and without --link every layer carries the
# /features/ directory as the stage last left it, so an edit to one feature
# would move every layer after it. Every platform feature under
# /features/<name>/ — sandbox-agent/features/<name>, and the files that live
# with what they describe — and the manifest in labels. FROM scratch because
# the agent applies it to a box rather than anything running it: no harness
# and no sandbox is an image (§10). COPY into scratch lays no whiteout, which a
# bundle must not carry (§2).
#
# One index, an entry per platform (§7): Linux's on amd64 and arm64, and
# darwin's on arm64 (ADR 26-11-11-963 §9), each the features its OS has and a
# manifest of its own. platform-features is what every OS's entry carries;
# platform-linux and platform-darwin add their OS's own, and `platform`
# picks the target's.
#
# Stablest first: an edit to one feature moves its layer and no other, and a
# box fetches only what moved.
FROM scratch AS platform-features

COPY --link sandbox-agent/features/build /features/build
COPY --link sandbox-agent/features/cli /features/cli
COPY --link sandbox-agent/features/python /features/python
COPY --link sandbox-agent/features/shells-editors /features/shells-editors
COPY --link sandbox-agent/features/direnv /features/direnv
COPY --link sandbox-agent/features/browser /features/browser
COPY --link sandbox-agent/features/nix /features/nix
COPY --link sandbox-agent/features/rust /features/rust
COPY --link sandbox-agent/features/node /features/node
COPY --link sandbox-agent/features/bun /features/bun
COPY --link sandbox-agent/features/mise /features/mise
COPY --link sandbox-agent/features/brew /features/brew
COPY --link sandbox-agent/features/go /features/go
COPY --link sandbox-agent/features/review /features/review
COPY --link sandbox-agent/features/discobox-cli /features/discobox-cli
COPY --link sandbox-agent/features/core /features/core
# Core's file that lives with what it describes: the discobox-access skill in
# the module whose flags it documents (ADR 0080 §3).
COPY --link access/skills/discobox-access /features/core/skills/discobox-access
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-access /features/core/discobox-access
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-review /features/review/discobox-review
COPY --link --from=discobox-cli-builder --chmod=0755 /out/discobox /features/discobox-cli/discobox

# Linux's entry: every feature, the desktop and nested Docker included, which
# are Linux container mechanisms (ADR 0145 §7).
FROM platform-features AS platform-linux
COPY --link sandbox-agent/features/desktop /features/desktop
# The mark and wordmark the viewer's page is drawn with, from the
# repository's own brand directory rather than a copy in this module: it is
# nested, so go:embed cannot reach up to them, and a second copy is a second
# thing to keep in step with Illustrator.
COPY --link assets/brand /features/desktop/brand
COPY --link sandbox-agent/features/docker /features/docker

# The platform's manifest layer: the env, volumes, groups, and seed files true
# of every box the platform provisions, rather than of any harness. A harness
# bundle does not inherit it — a harness extends a harness, never the platform
# — so the server composes it under the harness bundle's own layers
# (harness.ComposeBundleLabels, ADR 26-10-16-423 §2).
ARG IMAGE_LAYER_METADATA
# Required, and refused by the build itself rather than trusted: `LABEL
# key=${ARG}` with no argument passed sets the key to "" and builds a
# perfectly good bundle every harness is then rejected against. That is a
# build mistake, and it is only discoverable from a running server unless the
# build refuses it — which is how v0.4.0's harness images shipped. A FROM
# scratch stage has no shell to check it in, so the expansion does.
LABEL io.discobox.image.v1.10-platform="${IMAGE_LAYER_METADATA:?IMAGE_LAYER_METADATA build argument is required: pass --build-arg IMAGE_LAYER_METADATA=\"\$(jq -c . sandbox-agent/image.json)\"}"

# darwin's entry: the shared features alone. The desktop and nested Docker
# are absent, and its manifest, sandbox-agent/image.darwin.json, declares
# neither (ADR 0145 §7). Required as Linux's is, and only by a build that
# includes darwin.
FROM platform-features AS platform-darwin
ARG DARWIN_IMAGE_LAYER_METADATA
LABEL io.discobox.image.v1.10-platform="${DARWIN_IMAGE_LAYER_METADATA:?DARWIN_IMAGE_LAYER_METADATA build argument is required: pass --build-arg DARWIN_IMAGE_LAYER_METADATA=\"\$(jq -c . sandbox-agent/image.darwin.json)\"}"

# Each entry picks its OS's stage by the builtin TARGETOS, which BuildKit puts
# in the global scope itself. Never declare it there (`ARG TARGETOS` before the
# first FROM): an older Dockerfile frontend, such as the release's depot
# builders run, takes a bare declaration as an empty value over the builtin,
# and the build fails on the stage name "platform-" (#1803).
FROM platform-${TARGETOS} AS platform
