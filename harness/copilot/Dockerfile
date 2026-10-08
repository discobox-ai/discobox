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

# The npm package is a loader; the CLI itself is the platform package npm
# installs beside it as an optional dependency. On first run the CLI unpacks
# itself under ~/.cache/copilot/pkg, which the base layer already declares as
# per-user pool cache, so a pool pays that once per version rather than once
# per sandbox. npm's cache is build-time scratch.
RUN npm install -g @github/copilot \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox /etc/github-copilot/policy.d

# The agent this image installed, for the pool-cached version store
# (ADR 0114 §7). The store is generic; this file is the only harness-specific
# part of it.
COPY --chmod=0644 agent.conf /usr/local/libexec/discobox/agent.conf

# Copilot loads every file here as a *policy* hook source: read beside the
# user's own hooks rather than instead of them, and kept on by policy when
# the user's settings say disableAllHooks (ADR 26-10-02-840 §4).
COPY --chmod=0644 hooks.json /etc/github-copilot/policy.d/discobox.json

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-copilot
# The harness-run convention (ADR 0086 §3): the image installs its agent
# under this name, overwriting the base image's no-op shim, and the runtime
# types it. On PATH, not in libexec, because the runtime resolves it by name.
COPY --chmod=0755 launch.sh /usr/local/bin/discobox-harness-run

# The one-shot prompting interface in-sandbox tools ask for a model through
# (ADR 0079). On PATH, not in libexec: its callers resolve it by name.
COPY --chmod=0755 prompt.sh /usr/local/bin/discobox-prompt
