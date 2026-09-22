# syntax=mirror.gcr.io/docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}

# Nearly empty. Every other harness image installs its CLI and declares what it
# needs; this one's harness *is* the shell the base image already ships, and
# its env, volumes and groups are the base image's own layer, which this image
# inherits by being built FROM it (ADR 0086 §2).
#
# So there is no image.json and no label of its own: nothing is left to say. It
# is registered under the reserved `shell` slug, which is also what withholds
# the harness-run convention from it — a command typed into this terminal would
# be a second shell inside the first (ADR 0086 §3).
#
# What it does install is discobox-prompt, which every harness provides and
# discobox-access's command judge needs an answer from (ADR 0079). A shell runs
# no model, so this one answers the judge with a fixed verdict: refused, unless
# DISCOBOX_SHELL_JUDGE=allow. See prompt.sh.
COPY --chmod=0755 prompt.sh /usr/local/bin/discobox-prompt
