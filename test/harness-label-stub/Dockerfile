# syntax=docker/dockerfile:1.7

# A harness image with nothing in it but its metadata label.
#
# Seeding the built-in harness configs inspects each harness image for its
# io.discobox.image.v1 label and skips whatever it cannot find, so on a machine
# that has never built the real images every test that creates a sandbox fails
# with "harness config not found". CI points DISCOBOX_HARNESS_<SLUG>_IMAGE at
# these instead of building the sandbox-agent base plus three harness images on
# every pull request (ADR 0066 §7).
#
# Nothing can run this image, and nothing should: it has no filesystem. It
# exists so seeding has a label to read.

FROM scratch

ARG HARNESS_METADATA
LABEL io.discobox.image.v1=${HARNESS_METADATA}
