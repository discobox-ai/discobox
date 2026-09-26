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

# Oh My Pi (@oh-my-pi/pi-coding-agent), the package its docs install with npm
# or bun. Its entry point runs on bun, which the sandbox base image ships;
# npm's cache is build-time scratch.
RUN npm install -g @oh-my-pi/pi-coding-agent \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox /etc/omp

# The agent this image installed, for the pool-cached version store
# (ADR 0114 §7). The store is generic; this file is the only harness-specific
# part of it.
COPY --chmod=0644 agent.conf /usr/local/libexec/discobox/agent.conf

# omp's lifecycle reaches an extension and never a command, so this extension
# is what the claude-code and codex images declare as command hooks. The
# launcher names it with --hook: omp's configuration overlay below could name
# it too, but an overlay's `extensions` list replaces the user's rather than
# adding to it. Installed beside the configure script rather than on PATH
# because omp loads it by the path it is given, not by name.
COPY --chmod=0644 hook-extension.js /usr/local/libexec/discobox/omp-hook-extension.js

# omp's managed configuration layer: an overlay named by PI_CONFIG_FILES in the
# image's env, read after the user's global config and a project's, and the
# only layer the configure flow's capture cannot replace. It turns the startup
# update check off (the pool-cached store is the version source, ADR 0114) and
# sets the policy baseline.
COPY --chmod=0644 managed-config.yml /etc/omp/discobox.yml

# Imports the delivered OAuth credentials into omp's store. Both the launcher
# and the configure flow do this — the launcher for a sandbox, the configure
# flow to seed a reconfigure — so it is one script they share.
COPY --chmod=0755 import.sh /usr/local/libexec/discobox/omp-import-credentials

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-omp
# The harness-run convention (ADR 0086 §3): the image installs its agent
# under this name, overwriting the base image's no-op shim, and the runtime
# types it. On PATH, not in libexec, because the runtime resolves it by name.
COPY --chmod=0755 launch.sh /usr/local/bin/discobox-harness-run

# The one-shot prompting interface in-sandbox tools ask for a model through
# (ADR 0079). On PATH, not in libexec: its callers resolve it by name.
COPY --chmod=0755 prompt.sh /usr/local/bin/discobox-prompt
