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
COPY agentcreds ./agentcreds
COPY access ./access
COPY api ./api
COPY auditid ./auditid
COPY declared ./declared
COPY execstream ./execstream
COPY fsdurable ./fsdurable
COPY gitbackend ./gitbackend
COPY harness ./harness
COPY hostscope ./hostscope
COPY judge ./judge
COPY platform ./platform
COPY policy ./policy
COPY programstatus ./programstatus
COPY provisionspec ./provisionspec
COPY proxy ./proxy
COPY runcca ./runcca
COPY sandboxconfig ./sandboxconfig
COPY sandboxpath ./sandboxpath
COPY sandboxmeta ./sandboxmeta
COPY sandboxservices ./sandboxservices
COPY sandboxshell ./sandboxshell
COPY sandboxtree ./sandboxtree
COPY sandboxuser ./sandboxuser
COPY sourcecache ./sourcecache
COPY sourceref ./sourceref
COPY tarsums ./tarsums
COPY tools ./tools
COPY vsock ./vsock
COPY wire ./wire
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
ARG REVIEW_VERSION=v0.0.0-20261013064909-4f0ca443f7cf
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
COPY fsdurable ./fsdurable
COPY harness ./harness
COPY hostscope ./hostscope
COPY platform ./platform
COPY provisionspec ./provisionspec
COPY sandboxconfig ./sandboxconfig
COPY sandboxpath ./sandboxpath
COPY sandboxuser ./sandboxuser
COPY sourceref ./sourceref
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
COPY agentcreds ./agentcreds
COPY api ./api
COPY auditid ./auditid
COPY configfile ./configfile
COPY controlplane ./controlplane
COPY declared ./declared
COPY endpoint ./endpoint
COPY execstream ./execstream
COPY fsdurable ./fsdurable
COPY harness ./harness
COPY health ./health
COPY hostscope ./hostscope
COPY imagecache ./imagecache
COPY internal ./internal
COPY platform ./platform
COPY policy ./policy
COPY programstatus ./programstatus
COPY randomname ./randomname
COPY releasemanifest ./releasemanifest
COPY sandboxconfig ./sandboxconfig
COPY sandboxpath ./sandboxpath
COPY sandboxmeta ./sandboxmeta
COPY sandboxservices ./sandboxservices
COPY sandboxshell ./sandboxshell
COPY sandboxuser ./sandboxuser
COPY secretformat ./secretformat
COPY secretroute ./secretroute
COPY serverstage ./serverstage
COPY sourcecache ./sourcecache
COPY sourceref ./sourceref
COPY tools ./tools
COPY version ./version
COPY wellknown ./wellknown
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

# The platform bundle's layer label (the platform stage below), which nothing
# reads off this image: it is here so this image's digest, which a sandbox
# pins and whose move is what re-pins and upgrades it, moves with every edit
# to the layer the server composes each harness's manifest on. Required for
# the same reason it is there: an omitted argument fails the build rather than
# building an image whose identity misses the layer.
ARG IMAGE_LAYER_METADATA
LABEL io.discobox.image.v1.10-platform="${IMAGE_LAYER_METADATA:?IMAGE_LAYER_METADATA build argument is required: pass --build-arg IMAGE_LAYER_METADATA=\"\$(jq -c . sandbox-agent/image.json)\"}"
# The agent above has the export mode, which the pool agent checks for before
# it runs one (harness.TreeExportLabel, ADR 0129 §3).
LABEL io.discobox.tree-export.v1=tar
# It serves the sandbox's own repository, which the pool agent checks for
# before it forwards the worktree route here (harness.WorktreeGitLabel,
# ADR 0126 §4).
LABEL io.discobox.worktree-git.v1=smart-http

WORKDIR /workspace
# No VOLUME directives: discobox-wire.service wires all persistent and cached
# paths from the primary volumes the pool agent mounts, per ADR 0007. Anonymous
# volumes here would only shadow those binds.
EXPOSE 6080 6900
# systemd is PID 1, as it is on every sandbox (ADR 26-10-10-767). A debug shell
# overrides the entrypoint: `docker run --entrypoint bash`.
ENTRYPOINT ["/sbin/init"]

# The platform bundle (ADR 26-10-16-423 §7): every platform feature under
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

COPY sandbox-agent/features/build /features/build
COPY sandbox-agent/features/cli /features/cli
COPY sandbox-agent/features/python /features/python
COPY sandbox-agent/features/shells-editors /features/shells-editors
COPY sandbox-agent/features/direnv /features/direnv
COPY sandbox-agent/features/browser /features/browser
COPY sandbox-agent/features/desktop /features/desktop
COPY assets/brand /features/desktop/brand
COPY sandbox-agent/features/nix /features/nix
COPY sandbox-agent/features/rust /features/rust
COPY sandbox-agent/features/node /features/node
COPY sandbox-agent/features/bun /features/bun
COPY sandbox-agent/features/mise /features/mise
COPY sandbox-agent/features/brew /features/brew
COPY sandbox-agent/features/go /features/go
COPY sandbox-agent/features/docker /features/docker
COPY sandbox-agent/features/review /features/review
COPY sandbox-agent/features/discobox-cli /features/discobox-cli
COPY sandbox-agent/features/core /features/core
COPY harness/prompt-answer.sh /features/core/prompt-answer.sh
COPY access/skills/discobox-access /features/core/skills/discobox-access
COPY --from=sandbox-agent-builder --chmod=0755 /out/discobox-sandbox-agent /features/core/discobox-sandbox-agent
COPY --from=sandbox-agent-builder --chmod=0755 /out/discobox-access /features/core/discobox-access
COPY --from=sandbox-agent-builder --chmod=0755 /out/discobox-review /features/review/discobox-review
COPY --from=discobox-cli-builder --chmod=0755 /out/discobox /features/discobox-cli/discobox

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
