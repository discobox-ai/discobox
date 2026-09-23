# syntax=mirror.gcr.io/docker/dockerfile:1.7

# The image a libkrun pool boots, released on its own line as
# `discobox-vm-krun` (ADR 0148 §5): the shared guest's root disk beside the
# libkrun host runtime, so one pinned digest is everything a host needs.
#
# It compiles nothing. Both inputs are published images pinned here by digest:
# the guest from the `vm/v*` line, the runtime from `libkrun-runtime/v*`. A
# guest release therefore costs libkrun one publish of this and one pin edit,
# not a kernel compile. Moving either pin is the whole of a release on this
# line.
#
# GUEST_IMAGE is guestimage.DefaultVMImage, the guest vz boots too. Keep the
# two on the same digest unless a guest change is deliberately reaching one
# backend first.
ARG GUEST_IMAGE=ghcr.io/discobox-ai/discobox-vm@sha256:af1d6ee4ac0b833c7432f61651b29dacaad7a9c71e7e080ac3dfcdc1c8e46a48
# Not yet a digest: re-pinned to one once libkrun-runtime/v1 publishes, which
# `task vm:publish-libkrun-runtime` reports.
ARG RUNTIME_IMAGE=ghcr.io/discobox-ai/discobox-libkrun-runtime:v1

FROM --platform=linux/amd64 ${GUEST_IMAGE} AS guest
FROM --platform=linux/amd64 ${RUNTIME_IMAGE} AS runtime

FROM scratch
COPY --from=guest /root.ext4 /root.ext4
COPY --from=runtime /vmlinux /vmlinux
COPY --from=runtime /kernel.config /kernel.config
COPY --from=runtime /libkrun.so.1 /libkrun.so.1
COPY --from=runtime /passt /passt
