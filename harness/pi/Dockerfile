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

# pi looks for fd (or Debian's fdfind) and ripgrep on PATH, and downloads
# whichever is missing from GitHub on its first interactive run, into the
# home. The base image has ripgrep; fd-find is the other.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    set -eux \
    && apt-get update \
    && apt-get install -y --no-install-recommends fd-find

# pi is a Node script (`#!/usr/bin/env node`, Node 22.19 or newer) and runs on
# the Node the base image installs. npm's cache is build-time scratch.
RUN npm install -g @earendil-works/pi-coding-agent \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox

# The agent this image installed, for the pool-cached version store
# (ADR 0114 §7). The store is generic; this file is the only harness-specific
# part of it.
COPY --chmod=0644 agent.conf /usr/local/libexec/discobox/agent.conf

# pi's lifecycle reaches JavaScript and never a command, so this extension is
# what the other images declare as command hooks. pi has no system layer for
# extensions: the image's harness files put a one-line re-export of it in
# ~/.pi/agent/extensions, so the code stays this image's (ADR 26-10-15-259 §3).
COPY --chmod=0644 hook-extension.js /usr/local/libexec/discobox/pi-hook-extension.js

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-pi
# The harness-run convention (ADR 0086 §3): the image installs its agent
# under this name, overwriting the base image's no-op shim, and the runtime
# types it. On PATH, not in libexec, because the runtime resolves it by name.
COPY --chmod=0755 launch.sh /usr/local/bin/discobox-harness-run

# The one-shot prompting interface in-sandbox tools ask for a model through
# (ADR 0079). On PATH, not in libexec: its callers resolve it by name.
COPY --chmod=0755 prompt.sh /usr/local/bin/discobox-prompt
