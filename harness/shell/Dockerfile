# syntax=docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}

ARG HARNESS_METADATA
LABEL io.discobox.image.v1=${HARNESS_METADATA}

# Deliberately empty otherwise. Every other harness image installs its CLI here;
# this one's harness *is* the shell the base image already ships, so the image
# is its declarations and nothing else.
