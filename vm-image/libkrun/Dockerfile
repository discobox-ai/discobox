# syntax=mirror.gcr.io/docker/dockerfile:1.7

# The libkrun host runtime, released on its own line as
# `discobox-libkrun-runtime` (ADR 0148 §5): everything libkrun needs to boot a
# pool that is not the guest's root disk.
#
# - vmlinux, kernel.config: libkrunfw's patched Linux. libkrun boots a kernel
#   directly through libkrunfw's patched virtio-mmio and PVH entry paths, so a
#   distribution kernel does not boot under it at all.
# - libkrun.so.1: the library the launcher dlopens. Built from upstream, which
#   opens libkrunfw lazily and only when it is not handed a kernel, so nothing
#   here needs libkrunfw's own 21 MB copy of a kernel the provider never boots.
# - passt: the user-mode network the launcher execs, built static.
#
# They are one build because they move together: the libkrunfw patches are
# what this libkrun expects of its kernel. They are separate from the guest
# image because they move on a different clock from it, and this build
# compiles Linux. The image a libkrun pool boots packages this and the guest's
# root disk without compiling anything (package/Dockerfile).
#
# libkrun.so.1 and passt run on the host, not in the guest, so what they need
# of it is the whole of what a libkrun pool asks of a Linux machine beyond KVM.
# passt is static and asks nothing. libkrun is dlopened into the launcher, so
# it links the host's glibc and needs 2.34 or newer — Ubuntu 22.04, Debian 12,
# RHEL 9 and later — because that is where pthreads' symbols moved into libc,
# and no Debian that still gets security updates predates it. A host older than
# that cannot load it, which the server reports as a runtime that will not load
# (ADR 0148 §2). `vm:verify-libkrun-runtime` holds both lines. Nothing refers to a path
# on the machine that built it: the launcher finds both by path, so neither
# needs an install location.

FROM public.ecr.aws/docker/library/debian:13-slim AS kernel

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

COPY --chmod=0755 vm-image/libkrun/configure-kernel /usr/local/bin/configure-discobox-kernel
RUN configure-discobox-kernel linux-6.12.91 \
    && make -C linux-6.12.91 \
        -j"$(nproc)" \
        KBUILD_BUILD_TIMESTAMP="Mon Jun 1 14:28:39 UTC 2026" \
        KBUILD_BUILD_USER=root \
        KBUILD_BUILD_HOST=discobox \
        vmlinux

# Rust's own image on the same Debian rather than Debian's rustc: libkrun 1.19's
# dependencies need Rust 1.87, and Debian 13 ships 1.85.
FROM public.ecr.aws/docker/library/rust:1.88-slim-trixie@sha256:9a7159329166b45f453351a077367f501aa3e98378f7e327530e7966a139d05f AS libkrun

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        clang \
        libcap-ng-dev \
        libclang-dev \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

ADD --checksum=sha256:832e76e93f1ea7a41e5c763a9710acf42d3d54f628015c1255f115a4b7ef2a06 \
    https://github.com/containers/libkrun/archive/refs/tags/v1.19.0.tar.gz \
    /tmp/libkrun.tar.gz

WORKDIR /src
# BLK and NET are the two features the provider uses: raw virtio-block disks
# and a virtio-net device backed by passt's Unix socket. The build is
# --locked by the Makefile's own cargo invocation reading Cargo.lock.
RUN tar -xzf /tmp/libkrun.tar.gz --strip-components=1 \
    && make BLK=1 NET=1 \
    && install -D -m 0644 target/release/libkrun.so.1.19.0 /out/libkrun.so.1 \
    && strip --strip-unneeded /out/libkrun.so.1

FROM public.ecr.aws/docker/library/debian:13-slim AS passt

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

ADD --checksum=sha256:c5a916bdf084458ad791175aa2fe6f1984362524ada0cee84955ae144b6261cd \
    https://passt.top/passt/snapshot/passt-2025_09_19.623dbf6.tar.xz \
    /tmp/passt.tar.xz

WORKDIR /src
# Static, so the one executable here that is exec'd rather than dlopened brings
# no loader or library of the build machine with it.
RUN tar -xJf /tmp/passt.tar.xz --strip-components=1 \
    && make static VERSION=2025_09_19.623dbf6 \
    && install -D -m 0755 passt /out/passt \
    && strip /out/passt

FROM scratch
COPY --from=kernel /src/linux-6.12.91/vmlinux /vmlinux
COPY --from=kernel /src/linux-6.12.91/.config /kernel.config
COPY --from=libkrun /out/libkrun.so.1 /libkrun.so.1
COPY --from=passt /out/passt /passt
