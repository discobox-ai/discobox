# The Docker driver's sandbox base image (ADR 26-10-16-423 §10): the box a
# sandbox container is, before the platform is provisioned onto it. It is the
# Docker driver's equivalent of a provider's own image (exe.dev's exeuntu,
# boxd's), and holds what those have in common rather than what either alone
# has. Node, a browser, Nix, brew and the coding agents are features the
# platform provisions, and stay out of here so the platform cannot come to
# depend on them unnoticed. It is Debian 13 where those are Ubuntu: Debian's
# mirrors are the faster ones to build from, and the features provision onto
# either.
#
# The pool-agent image (pool-agent/Dockerfile) is built FROM it too, so a pool
# host that pulls both pulls these layers once. Two `docker build` runs never
# produce byte-identical apt layers, so a common parent image is the only thing
# that makes the layers shared blobs at pull time.

# Another Debian-family distribution can stand in, to provision the platform
# onto it as onto a provider's own image: `task build:ubuntu-platform`.
ARG DISTRO_IMAGE=public.ecr.aws/docker/library/debian:13-slim
FROM ${DISTRO_IMAGE}

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive

# Marks the image as Discobox's to reclaim once no container uses it. Every
# child inherits it through the image config, which is also how it survives a
# pull. See harness.ReclaimLabel and ADR 0040.
LABEL io.discobox.reclaimable.v1=true

# kvm first, at 992, before any package allocates system GIDs: a sandbox is
# privileged and sees the host's /dev/kvm with the host's numeric owner, so a
# sandbox user's kvm membership grants it only where the two agree, and which
# GID package installs leave kvm at moves with what they install.
RUN groupadd --system --gid 992 kvm

# systemd and the tools a working box is expected to have. apt's own
# cache-cleaning drop-in is removed: it fights the build caches children
# mount, and no image wants a container mutating its package state.
RUN set -eux \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        dbus \
        git \
        gnupg \
        iproute2 \
        iptables \
        jq \
        pipx \
        python3 \
        ripgrep \
        sudo \
        systemd \
        systemd-sysv \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /etc/apt/apt.conf.d/docker-clean

# Docker from Docker's own repository, with buildx and Compose v2,
# socket-activated: the daemon starts when something first connects to it.
# Kept apart from the layer above so a Docker update does not invalidate it.
# The pool agent enables docker.service outright (pool-agent/Dockerfile).
RUN set -eux \
    && install -m 0755 -d /etc/apt/keyrings \
    && . /etc/os-release \
    && curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
        "$ID" "$VERSION_CODENAME" "$(dpkg --print-architecture)" \
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
    && systemctl disable docker.service containerd.service \
    && systemctl enable docker.socket

# gh, from GitHub's own repository, which tracks its releases.
RUN set -eux \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Go and uv, each its upstream release, pinned with its checksum: Debian's Go
# trails upstream, and Debian packages no uv.
ARG GO_VERSION=1.27.3
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
ARG UV_VERSION=0.13.4
ARG UV_SHA256_AMD64=2b428396f12572d4429cf210ca234dd599a33b3af0958a4d5ed569d63e3f344f
ARG UV_SHA256_ARM64=1ae72b4718729e2ccc4f1ae2c6d1e531ca8467800e0546e9130b3886023a60ab
RUN set -eux \
    && arch="$(dpkg --print-architecture)" \
    && case "$arch" in \
        amd64) go_sha="$GO_SHA256_AMD64" uv_arch=x86_64 uv_sha="$UV_SHA256_AMD64" ;; \
        arm64) go_sha="$GO_SHA256_ARM64" uv_arch=aarch64 uv_sha="$UV_SHA256_ARM64" ;; \
        *) echo "unsupported architecture $arch" >&2; exit 1 ;; \
    esac \
    && tmp="$(mktemp -d)" \
    && curl -fsSL -o "$tmp/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" \
    && echo "$go_sha  $tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf "$tmp/go.tar.gz" \
    && curl -fsSL -o "$tmp/uv.tar.gz" "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uv_arch}-unknown-linux-gnu.tar.gz" \
    && echo "$uv_sha  $tmp/uv.tar.gz" | sha256sum -c - \
    && tar -C /usr/local/bin --strip-components=1 -xzf "$tmp/uv.tar.gz" "uv-${uv_arch}-unknown-linux-gnu/uv" "uv-${uv_arch}-unknown-linux-gnu/uvx" \
    && rm -rf "$tmp" \
    && /usr/local/go/bin/go version \
    && uv --version
ENV PATH=/usr/local/go/bin:${PATH}

# The login user, at uid 1000: passwordless sudo, in `docker`, and lingering.
# A sandbox whose account is another deletes this one at boot (ADR
# 26-10-16-423 §14). An account the distribution put there first (Ubuntu's
# `ubuntu`) gives way to it.
RUN set -eux \
    && existing="$(getent passwd 1000 | cut -d: -f1)" \
    && if [ -n "$existing" ]; then userdel --remove "$existing"; fi \
    && useradd --create-home --uid 1000 --user-group --shell /bin/bash --groups docker,sudo discobox \
    && echo 'discobox ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/discobox \
    && chmod 0440 /etc/sudoers.d/discobox \
    && mkdir -p /var/lib/systemd/linger \
    && touch /var/lib/systemd/linger/discobox

# Trim systemd down to what a container actually needs: exeuntu's masks (udev,
# getty, ssh, modules-load, remount-fs, sysusers, random-seed, apt-daily,
# unattended-upgrades) and the rest of what manages what the host owns. The
# host owns the kernel, hardware, partitions, and network namespace, so
# anything that manages those is dead weight or an outright failure at boot
# (systemd-modules-load left the pool image booting degraded). Periodic
# maintenance (apt, dpkg, fstrim, e2scrub) is masked so neither a pool nor a
# sandbox ever wakes up and mutates packages or filesystems behind the user.
#
# udev is masked, and nested Docker does not depend on it. A sandbox with the
# data volumes wired (image.json declares /var/lib/docker and
# /var/lib/containerd on `data`, without which nested Docker fails on
# overlay-on-overlay for reasons unrelated to udev) runs `docker run`, `docker
# build`, and HTTPS from inside a nested container with udev fully masked,
# reaching is-system-running=running with zero failed units.
#
# What udev would cost: systemd-udev-trigger replays a uevent for every device
# already in /sys, which in a container is the host's hardware — 786 of them
# on the machine this was measured on — and it is ordered
# Before=sysinit.target, so the sandbox agent and its primary terminal wait
# behind the whole enumeration. What it bought, measured by diffing /dev across
# a boot with and without it, was six symlink trees for host devices with no
# consumer in a container. Userspace boot went 2.08s -> 237ms.
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
        motd-news.timer \
        proc-sys-fs-binfmt_misc.mount \
        serial-getty@.service \
        ssh.service \
        ssh.socket \
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
        systemd-sysusers.service \
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
        systemd-volatile-root.service \
        unattended-upgrades.service

# Every container is its own machine: an empty machine-id, which systemd fills
# at first boot, and no SSH host keys for a second box to share.
RUN : > /etc/machine-id \
    && rm -f /var/lib/dbus/machine-id /etc/ssh/ssh_host_*

STOPSIGNAL SIGRTMIN+3
