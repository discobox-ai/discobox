ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local

# The bundle: the harness's own feature (provision.yaml) under
# /features/stub/, and its manifest in labels, composed by the server on the
# platform bundle's layer (ADR 26-10-16-423 §2). A box gets the feature from
# here, as every harness's does; the runnable twin below is run only for a
# sandbox delivered no platform.
FROM scratch AS bundle
ARG HARNESS_METADATA
# Required, and refused by the build itself rather than trusted: an omitted
# argument labels this bundle with the empty string, which only a running
# server would ever refuse. A FROM scratch stage has no shell to check it in,
# so the expansion does.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\"}"
COPY configure.sh provision.yaml /features/stub/

# The runnable twin, run only for a sandbox delivered no platform: the bundle's feature
# provisioned onto the sandbox agent image, as every harness's twin is. It is
# still what a harness registers beside its bundle until twins are retired.
FROM ${SANDBOX_AGENT_IMAGE} AS image
ARG HARNESS_METADATA
# The bundle's label again, which nothing reads off this image: it is here so
# this image's digest, which a sandbox pins and whose move is what re-pins and
# upgrades it, moves with every edit to the manifest the server composes from
# the bundle. The bundle stage this one builds refuses an empty argument.
LABEL io.discobox.image.v1="${HARNESS_METADATA}"
COPY --from=bundle /features/ /opt/discobox/bundles/harness/features/
RUN discobox-sandbox-agent provision /opt/discobox/bundles/platform /opt/discobox/bundles/harness

# The bundle is the default target, as every harness's is.
FROM bundle
