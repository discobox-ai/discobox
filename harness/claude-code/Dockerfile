# syntax=docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}

ARG HARNESS_METADATA
LABEL io.discobox.image.v1=${HARNESS_METADATA}

# npm's cache is build-time scratch. Left behind it put 100MB of it in this
# image, half the delta this layer is supposed to be.
RUN npm install -g @anthropic-ai/claude-code \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-claude-code
