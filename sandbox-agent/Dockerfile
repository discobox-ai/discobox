# Build natively on the host platform and cross-compile, so the platform
# bundle's every architecture is built with nothing emulated.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.27-trixie AS sandbox-agent-builder
ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY pkg/agentcreds ./pkg/agentcreds
COPY pkg/agentlayer ./pkg/agentlayer
COPY access ./access
COPY api ./api
COPY pkg/auditid ./pkg/auditid
COPY pkg/declared ./pkg/declared
COPY pkg/execstream ./pkg/execstream
COPY pkg/fsdurable ./pkg/fsdurable
COPY pkg/gitbackend ./pkg/gitbackend
COPY harness ./harness
COPY pkg/hostscope ./pkg/hostscope
COPY pkg/imagecache ./pkg/imagecache
COPY internal/filelock ./internal/filelock
COPY pkg/judge ./pkg/judge
COPY pkg/platform ./pkg/platform
COPY pkg/policy ./pkg/policy
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
COPY pkg/sourcecache ./pkg/sourcecache
COPY pkg/sourceref ./pkg/sourceref
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
# down: re-read `sandbox-agent/features/review/skills/discobox-review/SKILL.md` against
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
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.27-trixie AS discobox-cli-builder
ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY pkg/agentcreds ./pkg/agentcreds
COPY api ./api
COPY pkg/auditid ./pkg/auditid
COPY pkg/configfile ./pkg/configfile
COPY pkg/controlplane ./pkg/controlplane
COPY pkg/declared ./pkg/declared
COPY pkg/endpoint ./pkg/endpoint
COPY pkg/execstream ./pkg/execstream
COPY pkg/fsdurable ./pkg/fsdurable
COPY harness ./harness
COPY pkg/health ./pkg/health
COPY pkg/hostscope ./pkg/hostscope
COPY pkg/imagecache ./pkg/imagecache
COPY internal ./internal
COPY pkg/platform ./pkg/platform
COPY pkg/policy ./pkg/policy
COPY pkg/programstatus ./pkg/programstatus
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
COPY pkg/tools ./pkg/tools
COPY pkg/version ./pkg/version
COPY pkg/wellknown ./pkg/wellknown
COPY termpane ./termpane
COPY cli ./cli

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C cli build -trimpath -ldflags="-s -w" -o /out/discobox ./cmd/discobox

# The agent bundle (ADR 26-10-24-953 §2): the agent alone, one layer, which
# whoever makes a box places in it before the box applies its bundles — a pool
# in a sandbox, a provider in a pool — and is not in any box's list. FROM
# scratch, and one COPY, so the bundle is that one layer
# (agentlayer.AgentLayer). Built by name (`--target agent`).
FROM scratch AS agent
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-sandbox-agent /features/core/discobox-sandbox-agent

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
# Stablest first: an edit to one feature moves its layer and no other, and a
# box fetches only what moved.
FROM scratch AS platform

COPY --link sandbox-agent/features/build /features/build
COPY --link sandbox-agent/features/cli /features/cli
COPY --link sandbox-agent/features/python /features/python
COPY --link sandbox-agent/features/shells-editors /features/shells-editors
COPY --link sandbox-agent/features/direnv /features/direnv
COPY --link sandbox-agent/features/browser /features/browser
COPY --link sandbox-agent/features/desktop /features/desktop
# The mark and wordmark the viewer's page is drawn with, from the
# repository's own brand directory rather than a copy in this module: it is
# nested, so go:embed cannot reach up to them, and a second copy is a second
# thing to keep in step with Illustrator.
COPY --link assets/brand /features/desktop/brand
COPY --link sandbox-agent/features/nix /features/nix
COPY --link sandbox-agent/features/rust /features/rust
COPY --link sandbox-agent/features/node /features/node
COPY --link sandbox-agent/features/bun /features/bun
COPY --link sandbox-agent/features/mise /features/mise
COPY --link sandbox-agent/features/brew /features/brew
COPY --link sandbox-agent/features/go /features/go
COPY --link sandbox-agent/features/docker /features/docker
COPY --link sandbox-agent/features/review /features/review
COPY --link sandbox-agent/features/discobox-cli /features/discobox-cli
COPY --link sandbox-agent/features/core /features/core
# Core's files that live with what they describe: the prompt helper beside
# the harness wrappers it serves (harness/DESIGN.md), and the discobox-access
# skill in the module whose flags it documents (ADR 0080 §3).
COPY --link harness/prompt-answer.sh /features/core/prompt-answer.sh
COPY --link access/skills/discobox-access /features/core/skills/discobox-access
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-access /features/core/discobox-access
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-review /features/review/discobox-review
COPY --link --from=discobox-cli-builder --chmod=0755 /out/discobox /features/discobox-cli/discobox

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
