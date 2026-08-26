# syntax=docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}

ARG HARNESS_METADATA
LABEL io.discobox.image.v1=${HARNESS_METADATA}

# npm's cache is build-time scratch. Left behind it put 130MB of it in this
# image, half the delta this layer is supposed to be.
RUN npm install -g @openai/codex \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox /etc/codex

# Codex's system config layer: below the user's ~/.codex/config.toml, so these
# are defaults a user (or the configure flow's captured settings) can override,
# and above nothing the sandbox writes. The sandbox is itself the isolation
# boundary, so Codex's own approval prompts and inner sandbox would only ask
# about a machine that exists to be written to.
COPY --chmod=0644 system-config.toml /etc/codex/config.toml

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-codex
