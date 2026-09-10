# syntax=docker/dockerfile:1.7

# The libkrun guest kernel, released on its own line as `discobox-vm-kernel`.
#
# It is the one artifact a backend may not take from the shared guest image
# (../Dockerfile). libkrun boots a kernel directly through libkrunfw's patched
# virtio-mmio and PVH entry paths, so a distribution kernel does not boot under
# it at all — which is why this build exists and why nothing else in the project
# compiles Linux.
#
# It is separate from the guest image rather than a stage of it because the two
# have nothing to do with each other's inputs: this moves when libkrunfw or the
# upstream kernel does, the guest moves when Debian or Docker does, and folding
# them together would make every guest rebuild compile a kernel and every kernel
# bump republish a userland.
#
# The final stage carries the ELF vmlinux the launcher hands to
# krun_set_kernel, plus the config it was built with, so what booted can be read
# back off the artifact.

FROM debian:13-slim AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bc \
        binutils \
        bison \
        build-essential \
        ca-certificates \
        cpio \
        flex \
        libelf-dev \
        libssl-dev \
        patch \
        python3 \
        python3-pyelftools \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

ADD --checksum=sha256:94d99000c1431cdea9c586a410ca5ddb5c90db70640c80d4a8ec0e019613efd1 \
    https://github.com/containers/libkrunfw/archive/ec4b297964877d83432f9ccda6dad8ff6e9de3e4.tar.gz \
    /tmp/libkrunfw.tar.gz
ADD --checksum=sha256:0ff2ab9e169f9f1948557471fbb450d3018f8c5b77caf288e1a3982582597969 \
    https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.12.91.tar.xz \
    /tmp/linux-6.12.91.tar.xz

WORKDIR /src
RUN tar -xzf /tmp/libkrunfw.tar.gz --strip-components=1 \
    && mkdir -p tarballs \
    && ln -s /tmp/linux-6.12.91.tar.xz tarballs/linux-6.12.91.tar.xz \
    && make linux-6.12.91

COPY --chmod=0755 vm-image/kernel/configure-kernel /usr/local/bin/configure-discobox-kernel
RUN configure-discobox-kernel linux-6.12.91 \
    && make -C linux-6.12.91 \
        -j"$(nproc)" \
        KBUILD_BUILD_TIMESTAMP="Mon Jun 1 14:28:39 UTC 2026" \
        KBUILD_BUILD_USER=root \
        KBUILD_BUILD_HOST=discobox \
        vmlinux

FROM scratch
COPY --from=build /src/linux-6.12.91/vmlinux /vmlinux
COPY --from=build /src/linux-6.12.91/.config /kernel.config
