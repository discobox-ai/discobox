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
# upstream kernel does, the guest moves when Ubuntu or Docker does, and folding
# them together would make every guest rebuild compile a kernel and every kernel
# bump republish a userland.
#
# The final stage carries the ELF vmlinux the launcher hands to
# krun_set_kernel, plus the config it was built with, so what booted can be read
# back off the artifact.

# The build runs on the repository's one Ubuntu pin, resolute like every other
# Linux stage (see "Ubuntu" in the root DESIGN.md). Only the toolchain is
# Ubuntu's: the kernel it builds is libkrunfw's, whatever Ubuntu ships.
ARG UBUNTU_IMAGE=public.ecr.aws/docker/library/ubuntu:resolute-20261105.1@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db
ARG UBUNTU_MIRROR_AMD64=http://azure.archive.ubuntu.com/ubuntu
ARG UBUNTU_MIRROR_ARM64=http://azure.ports.ubuntu.com/ubuntu-ports

FROM ${UBUNTU_IMAGE} AS build
ARG UBUNTU_MIRROR_AMD64
ARG UBUNTU_MIRROR_ARM64

RUN set -eu \
    && case "$(dpkg --print-architecture)" in \
        amd64) mirror=$UBUNTU_MIRROR_AMD64 fallback=http://archive.ubuntu.com/ubuntu ;; \
        arm64) mirror=$UBUNTU_MIRROR_ARM64 fallback=http://ports.ubuntu.com/ubuntu-ports ;; \
        *) echo "no Ubuntu mirror for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && mkdir -p /etc/apt/mirrors \
    && printf '%s\tpriority:1\n%s\tpriority:2\n' "$mirror" "$fallback" > /etc/apt/mirrors/ubuntu.list \
    && printf '%s\n' 'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' '' \
        'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute-updates resolute-security' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' 'Valid-Until-Max: 604800' \
        > /etc/apt/sources.list.d/ubuntu.sources \
    && printf '%s\n' 'Acquire::Retries "5";' 'Acquire::http::Timeout "5";' > /etc/apt/apt.conf.d/80-discobox-apt \
    && echo "apt reads $mirror, then $fallback"
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

# Discobox's own patches, on top of libkrunfw's. libkrun has no power-off
# device and ends the VM only on a CPU reset, so without the first a guest's
# poweroff halts it and the VM runs on until the host kills it (#1319).
COPY vm-image/kernel/patches /tmp/discobox-patches
RUN for p in /tmp/discobox-patches/*.patch; do \
        patch -d linux-6.12.91 -p1 --forward --no-backup-if-mismatch < "$p" || exit 1; \
    done

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
