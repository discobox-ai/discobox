# syntax=docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}

ARG HARNESS_METADATA
LABEL io.discobox.image.v1=${HARNESS_METADATA}

# npm's cache is build-time scratch. Left behind it put 100MB of it in this
# image, half the delta this layer is supposed to be.
RUN npm install -g @anthropic-ai/claude-code \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox /etc/claude-code

COPY --chmod=0644 managed-settings.json /etc/claude-code/managed-settings.json
COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-claude-code
COPY --chmod=0755 launch.sh /usr/local/libexec/discobox/launch-claude-code

# The one-shot prompting interface in-sandbox tools ask for a model through
# (ADR 0079). On PATH, not in libexec: its callers resolve it by name.
COPY --chmod=0755 prompt.sh /usr/local/bin/discobox-prompt
