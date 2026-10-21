# syntax=mirror.gcr.io/docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local

# The bundle: the harness's own feature (provision.yaml) under
# /features/<name>/, and its manifest in labels, built FROM scratch because it
# is applied by the sandbox agent, never run (ADR 26-10-16-423 §2). A third
# party extends a harness by building FROM this one and adding a feature of
# its own beside it.
FROM scratch AS bundle
ARG HARNESS_METADATA
# Required, and refused by the build itself rather than trusted: an omitted
# argument labels this bundle with the empty string, which parses as a build
# that forgot rather than as a bundle declaring nothing, and only a running
# server would ever say so. A FROM scratch stage has no shell to check it in,
# so the expansion does.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\"}"
COPY agent.conf check-launcher.sh configure.sh hooks.json install-cli.sh launch.sh prompt.sh provision.yaml system-config.toml /features/codex-cli/

# The runnable twin a pool runs until the agent applies bundles itself: the
# bundle's features copied into the harness tree a box provisions from at boot
# and applied onto the platform's by the agent's runner, every step blocking
# (ADR 26-10-16-423 §§3–4). The platform's steps are recorded already, so only
# this feature's run.
FROM ${SANDBOX_AGENT_IMAGE} AS image
ARG HARNESS_METADATA
# The bundle's label again, which nothing reads off this image: it is here so
# this image's digest, which a sandbox pins and whose move is what re-pins and
# upgrades it, moves with every edit to the manifest the server composes from
# the bundle. The bundle stage this one builds refuses an empty argument.
LABEL io.discobox.image.v1="${HARNESS_METADATA}"
COPY --from=bundle /features/ /opt/discobox/bundles/harness/features/
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    discobox-sandbox-agent provision /opt/discobox/bundles/platform /opt/discobox/bundles/harness

# The bundle is the default target: it is what a harness is published as, and
# what a third party builds FROM.
FROM bundle
