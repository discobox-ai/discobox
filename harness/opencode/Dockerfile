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

# opencode 2 (@opencode/cli), ADR 26-10-15-848. npm's cache is build-time
# scratch.
RUN npm install -g @opencode/cli \
    && rm -rf /root/.npm \
    && install -d -m 0755 /usr/local/libexec/discobox /usr/local/libexec/discobox/opencode-hook-plugin

# The agent this image installed, for the pool-cached version store
# (ADR 0114 §7). The store is generic; this file is the only harness-specific
# part of it.
COPY --chmod=0644 agent.conf /usr/local/libexec/discobox/agent.conf

# opencode's lifecycle reaches plugins and never a command, so this plugin is
# what the other harness images declare as command hooks (ADR 0144). It is
# installed beside the configure script rather than on PATH because opencode
# resolves it by the absolute path the managed config names, not by name, and
# opencode 2 takes a plugin as a directory whose `server` module it loads.
# The directory is made 0755 above: COPY --chmod creates a missing parent with
# the file's mode, and a 0644 directory is one only root can read in — every
# other user's opencode then fails to load any plugin, its own providers too.
COPY --chmod=0644 hook-plugin/server.js /usr/local/libexec/discobox/opencode-hook-plugin/server.js
COPY --chmod=0644 hook-plugin/package.json /usr/local/libexec/discobox/opencode-hook-plugin/package.json

# The one owner of the image's credential entries (`own`) and of what was
# delivered (`delivered`, an opencode 1 auth.json included): the launcher's
# sync, configure and the judge all go through it (ADR 26-10-15-848 §3, §4).
COPY --chmod=0755 credentials.sh /usr/local/libexec/discobox/opencode-credentials
# The model a session starts on, for the judge and configure's check.
COPY --chmod=0755 model.sh /usr/local/libexec/discobox/opencode-model

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
