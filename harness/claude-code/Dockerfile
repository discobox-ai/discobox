# syntax=mirror.gcr.io/docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}

ARG HARNESS_METADATA
# Required for the same reason the base layer is (sandbox-agent/Dockerfile): an
# omitted argument labels this image with the empty string, which parses as a
# build that forgot rather than as an image declaring nothing, and only a
# running server would ever say so.
RUN [ -n "${HARNESS_METADATA}" ] || { \
        echo "HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\"" >&2; \
        exit 1; \
    }
LABEL io.discobox.image.v1=${HARNESS_METADATA}

# The harness's own feature (provision.yaml), copied into the harness tree a
# box provisions from at boot and applied onto the platform's by the agent's
# runner, every step blocking (ADR 26-10-16-423 §§3–4). The platform's steps
# are recorded already, so only this feature's run.
COPY agent.conf check-launcher.sh configure.sh install-cli.sh launch.sh managed-settings.json prompt.sh provision.yaml /opt/discobox/bundles/harness/features/claude-code/
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    discobox-sandbox-agent provision /opt/discobox/bundles/platform /opt/discobox/bundles/harness
