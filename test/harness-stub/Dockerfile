# syntax=mirror.gcr.io/docker/dockerfile:1.7

ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local

# The bundle: only the label. Nothing reads files from a stub's bundle — its
# configure script is installed into the runnable twin below — so, like the
# shell harness's, it holds no feature, and its manifest is image.json alone,
# composed by the server on the platform bundle's layer (ADR 26-10-16-423 §2).
FROM scratch AS bundle
ARG HARNESS_METADATA
# Required, and refused by the build itself rather than trusted: an omitted
# argument labels this bundle with the empty string, which only a running
# server would ever refuse. A FROM scratch stage has no shell to check it in,
# so the expansion does.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\"}"

# The runnable twin a pool runs: the sandbox agent image with the configure
# command image.json declares.
FROM ${SANDBOX_AGENT_IMAGE} AS image
ARG HARNESS_METADATA
# The bundle's label again, which nothing reads off this image: it is here so
# this image's digest, which a sandbox pins and whose move is what re-pins and
# upgrades it, moves with every edit to the manifest the server composes from
# the bundle. This stage does not build the bundle's, so it refuses an empty
# argument itself.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\"}"

RUN install -d -m 0755 /usr/local/libexec/discobox

COPY --chmod=0755 configure.sh /usr/local/libexec/discobox/configure-stub

# The bundle is the default target, as every harness's is.
FROM bundle
