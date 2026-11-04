FROM scratch
ARG HARNESS_METADATA
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA is required}"
COPY export.sh provision.yaml /features/ux-smoke/
