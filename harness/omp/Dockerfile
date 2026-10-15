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

# omp is a Bun script (`#!/usr/bin/env bun`) and runs on the Bun the base
# image installs; its native addon is an optional dependency npm picks for the
# platform. npm's cache is build-time scratch.
#
# One of its dependencies, onnxruntime-node, downloads onnxruntime's CUDA build
# from GitHub on install on linux-x64 — hundreds of megabytes for a GPU a
# sandbox does not have, and a download that has hung a build outright.
# ONNXRUNTIME_NODE_INSTALL_CUDA=skip is its own switch for that; the image's
# env sets it too, for the version store's installs.
RUN ONNXRUNTIME_NODE_INSTALL_CUDA=skip npm install -g @oh-my-pi/pi-coding-agent \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox /etc/omp

# The agent this image installed, for the pool-cached version store
# (ADR 0114 §7). The store is generic; this file is the only harness-specific
# part of it.
COPY --chmod=0644 agent.conf /usr/local/libexec/discobox/agent.conf

# omp's lifecycle reaches JavaScript and never a command, so this extension is
# what the other images declare as command hooks. omp has no system layer for
# extensions: the image's harness files put a one-line re-export of it in
# ~/.omp/agent/extensions, so the code stays this image's (ADR 26-10-14-718 §3).
COPY --chmod=0644 hook-extension.js /usr/local/libexec/discobox/omp-hook-extension.js

# The settings overlay the image's env names in PI_CONFIG_FILES: the policy
# baseline a configure capture cannot replace (ADR 26-10-14-718 §4).
COPY --chmod=0644 discobox.yml /etc/omp/discobox.yml

# PI_PROXY from HTTPS_PROXY, for omp's Anthropic transport, which reads no
# other proxy variable (ADR 26-10-14-718 §4). A login shell sources it; the
# launcher, configure command and discobox-prompt source it themselves.
COPY --chmod=0644 proxy-env.sh /etc/profile.d/omp-proxy.sh

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-omp
# The harness-run convention (ADR 0086 §3): the image installs its agent
# under this name, overwriting the base image's no-op shim, and the runtime
# types it. On PATH, not in libexec, because the runtime resolves it by name.
COPY --chmod=0755 launch.sh /usr/local/bin/discobox-harness-run

# The one-shot prompting interface in-sandbox tools ask for a model through
# (ADR 0079). On PATH, not in libexec: its callers resolve it by name.
COPY --chmod=0755 prompt.sh /usr/local/bin/discobox-prompt
