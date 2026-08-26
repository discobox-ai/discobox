# syntax=docker/dockerfile:1.7

# The base every Discobox systemd container image is built FROM: the pool agent
# (pool-agent/Dockerfile), the sandbox agent (sandbox-agent/Dockerfile), and
# through the latter every harness image.
#
# It exists so those images share layers rather than each rebuilding the same
# Debian, Docker, and systemd surface. Two `docker build` runs never produce
# byte-identical apt layers — apt writes timestamps and logs — so a common
# parent image is the only thing that makes the layers actually shared blobs at
# pull time. That is also why this image is pushed on release even though
# nothing ever runs it: its children must resolve the same reference at build
# time for their layers to match.
#
# What belongs here is what BOTH agents need. Anything one of them needs alone
# belongs in that agent's own Dockerfile, however tempting it is to hoist.

ARG DEBIAN_MIRROR=http://deb.debian.org/debian
ARG DEBIAN_SECURITY_MIRROR=http://security.debian.org/debian-security

FROM debian:13-slim

ARG DEBIAN_MIRROR
ARG DEBIAN_SECURITY_MIRROR

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive

# Marks the image as Discobox's to reclaim once no container uses it. Every
# child inherits it through the image config, which is also how it survives a
# pull. See harness.ReclaimLabel and ADR 0040.
LABEL io.discobox.reclaimable.v1=true

# The common runtime surface. Kept separate from the Docker layer below so a
# Docker version bump does not invalidate it.
RUN set -eux \
    && sed -i \
        -e "s|http://deb.debian.org/debian|${DEBIAN_MIRROR}|g" \
        -e "s|http://security.debian.org/debian-security|${DEBIAN_SECURITY_MIRROR}|g" \
        /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        dbus \
        git \
        gnupg \
        iproute2 \
        iptables \
        systemd \
        systemd-sysv \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Docker from Docker's own repository rather than Debian's docker.io, so both
# agents run the same daemon build from the same layer. The buildx and compose
# plugins are here because the sandbox needs them and the pool host is the
# machine that pulls both images: shared once costs less than installed twice.
#
# Neither enabling nor disabling any unit happens here — the pool agent runs
# dockerd directly while the sandbox socket-activates it, and that disagreement
# is the children's to declare.
RUN set -eux \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
        "$(grep VERSION_CODENAME /etc/os-release | cut -d= -f2)" \
        "$(dpkg --print-architecture)" \
        > /etc/apt/sources.list.d/docker.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        containerd.io \
        docker-buildx-plugin \
        docker-ce \
        docker-ce-cli \
        docker-compose-plugin \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    # apt's own cache-cleaning drop-in fights the build caches its children
    # mount, and neither image wants a container mutating its package state.
    && rm -f /etc/apt/apt.conf.d/docker-clean

# Trim systemd down to what a container actually needs. The host owns the
# kernel, hardware, partitions, and network namespace, so anything that manages
# those is dead weight or an outright failure at boot (systemd-modules-load left
# the pool image booting degraded). Periodic maintenance (apt, dpkg, fstrim,
# e2scrub) is masked so neither a pool nor a sandbox ever wakes up and mutates
# packages or filesystems behind the user.
#
# udev is masked, which an earlier revision of the sandbox image said it could
# not be: "nested Docker in the sandbox depends on it". Nothing does. A sandbox
# with the data volumes wired (the harness image.json declares /var/lib/docker
# and /var/lib/containerd on `data`, without which nested Docker fails on
# overlay-on-overlay for reasons unrelated to udev) runs `docker run`, `docker
# build`, and HTTPS from inside a nested container with udev fully masked,
# reaching is-system-running=running with zero failed units.
#
# The cost it was imposing: systemd-udev-trigger replays a uevent for every
# device already in /sys, which in a container is the host's hardware — 786 of
# them on the machine this was measured on — and it is ordered
# Before=sysinit.target, so the sandbox agent and its primary terminal wait
# behind the whole enumeration. What it bought, measured by diffing /dev across
# a boot with and without it, was six symlink trees for host devices: block,
# char, disk, ptp_hyperv, rtc, virtio-ports. The container's /dev is a
# Docker-provided tmpfs rather than devtmpfs, and nested Docker runs on
# overlayfs, so none of them had a consumer. Userspace boot went 2.08s -> 237ms.
RUN systemctl mask \
        apt-daily-upgrade.timer \
        apt-daily.timer \
        console-getty.service \
        dev-hugepages.mount \
        dpkg-db-backup.timer \
        e2scrub_all.timer \
        e2scrub_reap.service \
        fstrim.timer \
        getty.target \
        getty@.service \
        kmod-static-nodes.service \
        ldconfig.service \
        proc-sys-fs-binfmt_misc.mount \
        serial-getty@.service \
        sys-fs-fuse-connections.mount \
        systemd-ask-password-console.path \
        systemd-ask-password-console.service \
        systemd-ask-password-wall.path \
        systemd-ask-password-wall.service \
        systemd-battery-check.service \
        systemd-binfmt.service \
        systemd-boot-check-no-failures.service \
        systemd-bsod.service \
        systemd-confext.service \
        systemd-firstboot.service \
        systemd-growfs-root.service \
        systemd-hibernate-clear.service \
        systemd-hibernate-resume.service \
        systemd-hibernate.service \
        systemd-hostnamed.service \
        systemd-hostnamed.socket \
        systemd-hybrid-sleep.service \
        systemd-initctl.service \
        systemd-initctl.socket \
        systemd-journald-audit.socket \
        systemd-localed.service \
        systemd-logind.service \
        systemd-machine-id-commit.service \
        systemd-modules-load.service \
        systemd-networkd-wait-online.service \
        systemd-networkd.service \
        systemd-networkd.socket \
        systemd-pcrextend.socket \
        systemd-pcrlock.socket \
        systemd-pcrmachine.service \
        systemd-pcrphase-initrd.service \
        systemd-pcrphase-sysinit.service \
        systemd-pcrphase.service \
        systemd-pstore.service \
        systemd-quotacheck-root.service \
        systemd-random-seed.service \
        systemd-remount-fs.service \
        systemd-repart.service \
        systemd-rfkill.service \
        systemd-rfkill.socket \
        systemd-soft-reboot.service \
        systemd-suspend-then-hibernate.service \
        systemd-suspend.service \
        systemd-sysext.service \
        systemd-sysext.socket \
        systemd-timedated.service \
        systemd-tmpfiles-clean.timer \
        systemd-tpm2-setup-early.service \
        systemd-tpm2-setup.service \
        systemd-udev-settle.service \
        systemd-udev-trigger.service \
        systemd-udevd-control.socket \
        systemd-udevd-kernel.socket \
        systemd-udevd.service \
        systemd-update-utmp-runlevel.service \
        systemd-update-utmp.service \
        systemd-volatile-root.service

STOPSIGNAL SIGRTMIN+3
