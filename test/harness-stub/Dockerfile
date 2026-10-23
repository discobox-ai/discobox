# The bundle: the harness's own feature (provision.yaml) under
# /features/stub/, and its manifest in labels, composed by the server on the
# platform bundle's layer (ADR 26-10-16-423 §2). A box gets the feature from
# here, as every harness's does: no harness is an image (§10).
FROM scratch AS bundle
ARG HARNESS_METADATA
# Required, and refused by the build itself rather than trusted: an omitted
# argument labels this bundle with the empty string, which only a running
# server would ever refuse. A FROM scratch stage has no shell to check it in,
# so the expansion does.
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required: pass --build-arg HARNESS_METADATA=\"\$(jq -c . image.json)\"}"
COPY configure.sh provision.yaml /features/stub/
