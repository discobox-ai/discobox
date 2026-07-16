# syntax=docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}

ARG HARNESS_METADATA
LABEL io.discobox.harness.v1=${HARNESS_METADATA}

RUN npm install -g @openai/codex \
    && install -d -m 0755 /usr/local/libexec/discobox

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-codex
COPY image.json /usr/share/discobox/image.json
