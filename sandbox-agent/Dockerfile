# Declared before the first FROM so it is a global build argument the runtime
# stage's FROM can expand; declared alongside that FROM it would belong to the
# builder stage and resolve to nothing.
ARG BASE_IMAGE=discobox-sandbox-base:local

# Build natively on the host platform and cross-compile, so multi-arch image
# builds only emulate the (apt-based) runtime stage, not the Go compiler.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.27-trixie AS sandbox-agent-builder
ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY pkg/agentcreds ./pkg/agentcreds
COPY pkg/agentlayer ./pkg/agentlayer
COPY access ./access
COPY pkg/ahphost ./pkg/ahphost
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
ARG REVIEW_VERSION=v0.0.0-20261022000229-b39a99befe76
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

# The provisioner every feature below is applied with: the provision package
# behind a command of its own (cmd/discobox-image-provision). Its own stage, so
# it copies only what that package imports: a step's cache follows the binary
# it mounts, and this one is the same bytes until provisioning changes, where
# the agent changes with nearly every edit to its module.
FROM --platform=$BUILDPLATFORM public.ecr.aws/docker/library/golang:1.27-trixie AS image-provision-builder
ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY pkg/fsdurable ./pkg/fsdurable
COPY harness ./harness
COPY pkg/hostscope ./pkg/hostscope
COPY pkg/platform ./pkg/platform
COPY pkg/provisionspec ./pkg/provisionspec
COPY pkg/sandboxconfig ./pkg/sandboxconfig
COPY pkg/sandboxpath ./pkg/sandboxpath
COPY pkg/sandboxuser ./pkg/sandboxuser
COPY pkg/sourceref ./pkg/sourceref
COPY sandbox-agent ./sandbox-agent

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-image-provision ./cmd/discobox-image-provision

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
COPY pkg/ahphost ./pkg/ahphost
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

# Debian, Docker, and the trimmed systemd unit set come from the Docker
# driver's sandbox base image (ADR 26-10-16-423 §10), which the pool image is
# also built FROM so a pool host that pulls both pulls those layers once. See
# server/providers/dockerworker/sandbox-image/Dockerfile.
#
# Everything else is the platform's features (ADR 26-10-16-423 §§3–4):
# sandbox-agent/features/<name>, copied into the platform tree a box
# provisions from at boot and applied here by the runner, every step blocking.
# The tree stays in the image, and so do the runner's records of what it
# applied, so a box's boot finds every step done and turns off what its
# project opted out of.
#
# One feature per layer, stablest first, each after those it requires: a
# provisioning step skips what an earlier layer recorded, so editing one
# feature applies that feature and the ones after it, and not the 4GB before.
#
# A pool runs this image, through a harness's runnable twin built FROM it,
# only for a sandbox delivered no platform: one with a pin that is not a
# content digest, a build-mode development image's. A delivered sandbox's
# container starts from the sandbox base image instead, with the agent layer
# and units placed in it, and fetches its bundles (ADR 26-10-16-423 §7). This
# image lays the same units itself, below, and runs its agent from its own
# tree through the same /opt/discobox/current.
FROM ${BASE_IMAGE} AS sandbox-agent

ENV NIX_CONFIG="experimental-features = nix-command flakes"
ENV NIX_REMOTE=daemon
ENV PATH=/nix/var/nix/profiles/default/bin:/usr/local/go/bin:/usr/local/bin:${PATH}

COPY sandbox-agent/features/build /opt/discobox/bundles/platform/features/build
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/cli /opt/discobox/bundles/platform/features/cli
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/python /opt/discobox/bundles/platform/features/python
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/shells-editors /opt/discobox/bundles/platform/features/shells-editors
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/direnv /opt/discobox/bundles/platform/features/direnv
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/browser /opt/discobox/bundles/platform/features/browser
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/desktop /opt/discobox/bundles/platform/features/desktop
# The mark and wordmark the viewer's page is drawn with, from the
# repository's own brand directory rather than a copy in this module: it is
# nested, so go:embed cannot reach up to them, and a second copy is a second
# thing to keep in step with Illustrator.
COPY assets/brand /opt/discobox/bundles/platform/features/desktop/brand
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/nix /opt/discobox/bundles/platform/features/nix
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/rust /opt/discobox/bundles/platform/features/rust
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/node /opt/discobox/bundles/platform/features/node
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/bun /opt/discobox/bundles/platform/features/bun
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/mise /opt/discobox/bundles/platform/features/mise
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/brew /opt/discobox/bundles/platform/features/brew
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/go /opt/discobox/bundles/platform/features/go
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/docker /opt/discobox/bundles/platform/features/docker
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/review /opt/discobox/bundles/platform/features/review
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/discobox-cli /opt/discobox/bundles/platform/features/discobox-cli
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

COPY sandbox-agent/features/core /opt/discobox/bundles/platform/features/core
# Core's files that live with what they describe: the prompt helper beside
# the harness wrappers it serves (harness/DESIGN.md), and the discobox-access
# skill in the module whose flags it documents (ADR 0080 §3).
COPY harness/prompt-answer.sh /opt/discobox/bundles/platform/features/core/prompt-answer.sh
COPY access/skills/discobox-access /opt/discobox/bundles/platform/features/core/skills/discobox-access
RUN --mount=type=bind,from=image-provision-builder,source=/out/discobox-image-provision,target=/tmp/discobox-image-provision \
    --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    /tmp/discobox-image-provision /opt/discobox/bundles/platform

# The binaries last, since they change most: each feature that runs one links
# it from here (ADR 26-10-16-423 §7), so a new build is a new layer of files
# and no provisioning.
COPY --from=sandbox-agent-builder --chmod=0755 /out/discobox-sandbox-agent /opt/discobox/bundles/platform/features/core/discobox-sandbox-agent
COPY --from=sandbox-agent-builder --chmod=0755 /out/discobox-access /opt/discobox/bundles/platform/features/core/discobox-access
COPY --from=sandbox-agent-builder --chmod=0755 /out/discobox-review /opt/discobox/bundles/platform/features/review/discobox-review
COPY --from=discobox-cli-builder --chmod=0755 /out/discobox /opt/discobox/bundles/platform/features/discobox-cli/discobox
# The agent's units, the ones a pool places in a delivered box, run the agent
# from /opt/discobox/current; here that is the platform tree, which holds the
# agent at the same path its layer does.
RUN ln -s bundles/platform /opt/discobox/current \
    && /opt/discobox/current/features/core/discobox-sandbox-agent install

# The platform bundle's layer label (the platform stage below), which nothing
# reads off this image: it is here so this image's digest, which a sandbox
# pins and whose move is what re-pins and upgrades it, moves with every edit
# to the layer the server composes each harness's manifest on. Required for
# the same reason it is there: an omitted argument fails the build rather than
# building an image whose identity misses the layer.
ARG IMAGE_LAYER_METADATA
LABEL io.discobox.image.v1.10-platform="${IMAGE_LAYER_METADATA:?IMAGE_LAYER_METADATA build argument is required: pass --build-arg IMAGE_LAYER_METADATA=\"\$(jq -c . sandbox-agent/image.json)\"}"

WORKDIR /workspace
# No VOLUME directives: discobox-wire.service wires all persistent and cached
# paths from the primary volumes the pool agent mounts, per ADR 0007. Anonymous
# volumes here would only shadow those binds.
EXPOSE 6080 6900
# systemd is PID 1, as it is on every sandbox (ADR 26-10-10-767). A debug shell
# overrides the entrypoint: `docker run --entrypoint bash`.
ENTRYPOINT ["/sbin/init"]

# The platform bundle (ADR 26-10-16-423 §7), each COPY --link: a box fetches
# only the layers its store lacks, and without --link every layer carries the
# /features/ directory as the stage last left it, so an edit to one feature
# would move every layer after it. Every platform feature under
# /features/<name>/, the same trees the stage above copies into
# /opt/discobox/bundles/platform/features/ and provisions, and the manifest in
# labels. FROM scratch because the agent applies it rather than anything
# running it, and copied from the build context and the builder stages rather
# than from the stage above, so building it never builds the 4GB image. COPY
# into scratch lays no whiteout, which a bundle must not carry (§2).
#
# The last stage, so it is the default target: the runnable image is built
# with --target sandbox-agent.
FROM scratch AS platform

COPY --link sandbox-agent/features/build /features/build
COPY --link sandbox-agent/features/cli /features/cli
COPY --link sandbox-agent/features/python /features/python
COPY --link sandbox-agent/features/shells-editors /features/shells-editors
COPY --link sandbox-agent/features/direnv /features/direnv
COPY --link sandbox-agent/features/browser /features/browser
COPY --link sandbox-agent/features/desktop /features/desktop
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
COPY --link harness/prompt-answer.sh /features/core/prompt-answer.sh
COPY --link access/skills/discobox-access /features/core/skills/discobox-access
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-access /features/core/discobox-access
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-review /features/review/discobox-review
COPY --link --from=discobox-cli-builder --chmod=0755 /out/discobox /features/discobox-cli/discobox
# The agent layer: the agent alone, and the bundle's last layer, which is how
# a pool finds it to place in a box before the box boots (agentlayer.AgentLayer,
# ADR 26-10-16-423 §7). Keep it last and keep it one COPY.
COPY --link --from=sandbox-agent-builder --chmod=0755 /out/discobox-sandbox-agent /features/core/discobox-sandbox-agent

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
