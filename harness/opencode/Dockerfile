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

# opencode 1 (opencode-ai), the release its own installer and docs install.
# opencode 2 is a different package (@opencode/cli) with a different credential
# store. npm's cache is build-time scratch.
RUN npm install -g opencode-ai \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox

# The agent this image installed, for the pool-cached version store
# (ADR 0114 §7). The store is generic; this file is the only harness-specific
# part of it.
COPY --chmod=0644 agent.conf /usr/local/libexec/discobox/agent.conf

# opencode's lifecycle reaches JavaScript and never a command, so this plugin
# is what the other harness images declare as command hooks (ADR 0144). It is
# installed beside the configure script rather than on PATH because opencode
# resolves it by the absolute path the managed config names, not by name.
COPY --chmod=0644 hook-plugin.js /usr/local/libexec/discobox/opencode-hook-plugin.js

# opencode's managed configuration layer, read after the user's own and after a
# project's, and the only layer the configure flow's capture cannot replace.
# Plugin entries accumulate across layers, so naming this one never drops a
# plugin the user added.
RUN install -d -m 0755 /etc/opencode
COPY --chmod=0644 managed-config.json /etc/opencode/opencode.json

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-opencode
# The harness-run convention (ADR 0086 §3): the image installs its agent
# under this name, overwriting the base image's no-op shim, and the runtime
# types it. On PATH, not in libexec, because the runtime resolves it by name.
COPY --chmod=0755 launch.sh /usr/local/bin/discobox-harness-run

# The one-shot prompting interface in-sandbox tools ask for a model through
# (ADR 0079). On PATH, not in libexec: its callers resolve it by name.
COPY --chmod=0755 prompt.sh /usr/local/bin/discobox-prompt
