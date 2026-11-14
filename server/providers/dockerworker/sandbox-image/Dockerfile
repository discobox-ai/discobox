# The Docker driver's sandbox base image (ADR 26-10-16-423 §10): the box a
# sandbox container is, before the platform is provisioned onto it. It is the
# Docker driver's equivalent of a provider's own image (exe.dev's exeuntu,
# boxd's), and holds what those have in common rather than what either alone
# has. Node, a browser, Nix, brew and the coding agents are features the
# platform provisions, and stay out of here so the platform cannot come to
# depend on them unnoticed. It is Debian sid where those are Ubuntu: sid is
# the one Debian this repository builds on (see "Debian" in the root
# DESIGN.md), and the features provision onto either.
#
# A Docker pool's container runs it too, provisioned from the pool bundle (ADR
# 26-10-24-953 §2), so a pool host pulls one image for itself and its
# sandboxes.

# The repository's one Debian pin: sid's image by digest, and the
# snapshot.debian.org timestamp apt builds from. `go tool task debian:bump`
# moves both, here and in every other Dockerfile.
ARG DEBIAN_IMAGE=public.ecr.aws/docker/library/debian:sid-20261104-slim@sha256:07a3205e8583fdff12d2b05bd030f50437e9fd340ee616df0c05dc77af75767b
ARG DEBIAN_SNAPSHOT=20261111T202414Z

# Another Debian-family distribution can stand in, to provision the platform
# onto it as onto a provider's own image: `task build:ubuntu-sandbox-base`.
ARG DISTRO_IMAGE=${DEBIAN_IMAGE}
FROM ${DISTRO_IMAGE}
ARG DEBIAN_SNAPSHOT

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

# On Debian, apt builds this image from the pinned snapshot, so a rebuild
# installs what the last one did; the last apt layer below hands the box back
# to the live sid archive, and takes away the snapshot's check below. The
# snapshot is read over http: the image has no CA certificates to check TLS
# with until apt installs them. apt verifies every index against Debian's
# archive key, but with Check-Valid-Until off (a snapshot's Release expires a
# week after it was taken) any older signed index would pass for it, so an
# apt hook fails every update whose sid index is not dated within the day
# before the snapshot. A stand-in distribution keeps its own sources.
RUN set -eux \
    && . /etc/os-release \
    && if [ "$ID" = debian ]; then \
        printf '%s\n' 'Types: deb' "URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}" \
            'Suites: sid' 'Components: main' 'Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp' \
            'Check-Valid-Until: no' > /etc/apt/sources.list.d/debian.sources; \
        s=$(date -u -d "$(echo "$DEBIAN_SNAPSHOT" | sed -E 's/^(.{4})(..)(..)T(..)(..)(..)Z$/\1-\2-\3 \4:\5:\6 UTC/')" +%s); \
        printf '%s\n' '#!/bin/sh' \
            'set -eu' \
            "released=\$(sed -n 's/^Date: //p' /var/lib/apt/lists/*_dists_sid_InRelease /var/lib/apt/lists/*_dists_sid_Release 2>/dev/null | head -n 1)" \
            "[ -n \"\$released\" ] || { echo 'apt fetched no signed sid Release to check against snapshot ${DEBIAN_SNAPSHOT}: refusing it' >&2; exit 1; }" \
            "d=\$(date -u -d \"\$released\" +%s)" \
            "[ \"\$d\" -le $s ] && [ \"\$d\" -gt $((s - 86400)) ] && exit 0" \
            "echo \"the sid index apt fetched is dated \$released, not snapshot ${DEBIAN_SNAPSHOT}'s: refusing it\" >&2" \
            'exit 1' > /usr/local/sbin/discobox-apt-snapshot; \
        chmod 0755 /usr/local/sbin/discobox-apt-snapshot; \
        echo 'APT::Update::Post-Invoke-Success { "/usr/local/sbin/discobox-apt-snapshot"; };' > /etc/apt/apt.conf.d/81-discobox-snapshot; \
    fi \
    && echo 'Acquire::Retries "5";' > /etc/apt/apt.conf.d/80-discobox-retries

# Common apt requirements of core, build, cli, python and shells-editors
# (ADR 26-11-06-664). Keep in sync with their provision.yaml declarations.
# This layer precedes all application inputs; binaries live in separate bundles.
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
        expect \
        file \
        git \
        gnupg \
        iproute2 \
        iptables \
        jq \
        less \
        openssh-client \
        openssh-sftp-server \
        passwd \
        patch \
        pipx \
        poppler-utils \
        procps \
        psmisc \
        python-is-python3 \
        python3 \
        python3-pip \
        python3-requests \
        python3-venv \
        ripgrep \
        screen \
        shellcheck \
        socat \
        sqlite3 \
        sudo \
        systemd \
        systemd-sysv \
        tmux \
        unzip \
        vim \
        xz-utils \
        zsh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f /etc/apt/apt.conf.d/docker-clean

# Docker from Docker's own repository, with buildx and Compose v2,
# socket-activated: the daemon starts when something first connects to it.
# Kept apart from the layer above so a Docker update does not invalidate it.
# A pool's container runs none of it: its daemon's socket is bound in.
#
# Docker publishes no suite for Debian sid, so on Debian it is Docker's trixie
# suite: the one thing in this repository's builds that names trixie, and the
# owner's choice over sid's own older docker.io. Docker's packages are its own
# builds with few library dependencies, which sid's newer libraries satisfy.
# Everything else in the image is sid's.
ARG DOCKER_DEBIAN_SUITE=trixie
RUN set -eux \
    && install -m 0755 -d /etc/apt/keyrings \
    && . /etc/os-release \
    && suite="$VERSION_CODENAME" \
    && if [ "$ID" = debian ]; then suite="$DOCKER_DEBIAN_SUITE"; fi \
    && curl -fsSL --retry 5 --retry-all-errors "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
        "$ID" "$suite" "$(dpkg --print-architecture)" \
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

# gh, from GitHub's own repository, which tracks its releases. The last apt
# layer, so it is also where Debian's sources go back from the snapshot to the
# live sid archive: a box's own apt, and the features provisioned onto it,
# install today's sid.
RUN set -eux \
    && curl -fsSL --retry 5 --retry-all-errors https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && . /etc/os-release \
    && if [ "$ID" = debian ]; then \
        printf '%s\n' 'Types: deb' 'URIs: http://deb.debian.org/debian' \
            'Suites: sid' 'Components: main' 'Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp' \
            > /etc/apt/sources.list.d/debian.sources; \
        rm -f /etc/apt/apt.conf.d/81-discobox-snapshot /usr/local/sbin/discobox-apt-snapshot; \
    fi

# Go and uv, each its upstream release, pinned with its checksum: Debian's Go
# trails upstream, and Debian packages no uv.
# Every curl in this image retries, any error included: one 504 from a release
# host would otherwise fail the build (#871). curl's own backoff (1s doubling,
# about 30s over five retries) rides out a short outage. curl -o discards a
# failed attempt's partial file before retrying, and the checksum checks what
# it kept.
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
    && curl -fsSL --retry 5 --retry-all-errors -o "$tmp/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" \
    && echo "$go_sha  $tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf "$tmp/go.tar.gz" \
    && curl -fsSL --retry 5 --retry-all-errors -o "$tmp/uv.tar.gz" "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uv_arch}-unknown-linux-gnu.tar.gz" \
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
