# The Docker driver's sandbox base image (ADR 26-10-16-423 §10): the box a
# sandbox container is, before the platform is provisioned onto it. It is the
# Docker driver's equivalent of a provider's own image (exe.dev's exeuntu,
# boxd's), and holds what those have in common rather than what either alone
# has. Node, a browser, Nix, brew and the coding agents are features the
# platform provisions, and stay out of here so the platform cannot come to
# depend on them unnoticed. It is Ubuntu 26.04 LTS (resolute), the one Linux
# this repository builds on (see "Ubuntu" in the root DESIGN.md), and the
# features provision onto any Debian-family image.
#
# A Docker pool's container runs it too, provisioned from the pool bundle (ADR
# 26-10-24-953 §2), so a pool host pulls one image for itself and its
# sandboxes. An archil machine boots a release's copy of it as a VM guest
# (discovm.DefaultOCIBase, ADR 26-11-16-619 §4): keep systemd, dbus and
# nftables (named below, not left to docker-ce's dependency on it), and see
# server/providers/REVIEW.md before masking a unit.

# The repository's one Ubuntu pin: resolute's image by digest, and the mirror
# apt reads first on each architecture. `go tool task ubuntu:bump` moves the
# image, here and in every other Dockerfile.
ARG UBUNTU_IMAGE=public.ecr.aws/docker/library/ubuntu:resolute-20261105.1@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db
ARG UBUNTU_MIRROR_AMD64=http://mirrors.edge.kernel.org/ubuntu
ARG UBUNTU_MIRROR_ARM64=http://mirrors.mit.edu/ubuntu-ports

FROM ${UBUNTU_IMAGE}
ARG UBUNTU_MIRROR_AMD64
ARG UBUNTU_MIRROR_ARM64

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

# apt reads resolute's live pockets (resolute, -updates, -security) from the
# ordered mirror list during the build and from inside the box, including
# during feature provisioning. Every entry is plain http: the
# image has no CA certificates to check TLS with until apt installs them, and
# apt verifies every index against Ubuntu's archive key, whichever host served
# it. Ubuntu's Release files carry no Valid-Until, so the moving pockets
# (-updates, -security) get Valid-Until-Max: apt refuses one dated more than a
# week ago, so no host can replay an older, validly signed index of them, in
# the build or in a box.
RUN set -eu \
    && case "$(dpkg --print-architecture)" in \
        amd64) first=$UBUNTU_MIRROR_AMD64 second=http://ftp.osuosl.org/pub/ubuntu third=http://azure.archive.ubuntu.com/ubuntu ;; \
        arm64) first=$UBUNTU_MIRROR_ARM64 second=http://azure.ports.ubuntu.com/ubuntu-ports third=http://ports.ubuntu.com/ubuntu-ports ;; \
        *) echo "no Ubuntu mirror for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac \
    && mkdir -p /etc/apt/mirrors \
    && printf '%s\tpriority:1\n%s\tpriority:2\n%s\tpriority:3\n' "$first" "$second" "$third" > /etc/apt/mirrors/ubuntu.list \
    && printf '%s\n' 'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' '' \
        'Types: deb' 'URIs: mirror+file:/etc/apt/mirrors/ubuntu.list' \
        'Suites: resolute-updates resolute-security' 'Components: main universe' \
        'Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg' 'Valid-Until-Max: 604800' \
        > /etc/apt/sources.list.d/ubuntu.sources \
    && printf '%s\n' 'Acquire::Retries "5";' 'Acquire::http::Timeout "5";' > /etc/apt/apt.conf.d/80-discobox-apt \
    && echo "apt reads $first, then $second, then $third"

# Common apt requirements of core, build, cli, python and shells-editors
# (ADR 26-11-06-664). Keep in sync with their provision.yaml declarations.
# This layer precedes all application inputs; binaries live in separate bundles.
# systemd and the tools a working box is expected to have. apt's own
# cache-cleaning drop-in is removed: it fights the build caches children
# mount, and no image wants a container mutating its package state.
RUN set -eux \
    && cat /etc/apt/mirrors/ubuntu.list \
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
        nftables \
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
# It is Docker's resolute suite, not Ubuntu's docker.io.
#
# The daemon is pinned, and the pool VM guest runs the same one:
# DOCKER_CE_VERSION and CONTAINERD_IO_VERSION are one pin with vm-image's,
# kept one by `go tool task check:ubuntu` and moved by `go tool task
# docker:bump`.
# A pin names the upstream version and the build takes the suite's package of
# it. buildx and Compose are the suite's newest. Docker's key is served beside
# the packages it signs, so it is held to Docker's published fingerprint.
ARG DOCKER_CE_VERSION=29.9.3-1
ARG CONTAINERD_IO_VERSION=2.4.2-4
ARG DOCKER_APT_KEY_FINGERPRINT=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
RUN set -eux \
    && install -m 0755 -d /etc/apt/keyrings \
    && . /etc/os-release \
    && curl -fsSL --retry 5 --retry-all-errors https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && gnupg=$(mktemp -d) \
    && fingerprints=$(GNUPGHOME=$gnupg gpg --show-keys --with-colons /etc/apt/keyrings/docker.asc | awk -F: '$1 == "pub" { primary = 1 } $1 == "fpr" && primary { print $10; primary = 0 }') \
    && rm -rf "$gnupg" \
    && if [ "$fingerprints" != "$DOCKER_APT_KEY_FINGERPRINT" ]; then \
        echo "Docker's apt key is not $DOCKER_APT_KEY_FINGERPRINT: $fingerprints" >&2; exit 1; \
    fi \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' \
        "$VERSION_CODENAME" "$(dpkg --print-architecture)" \
        > /etc/apt/sources.list.d/docker.sources \
    && cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && ce=$(apt-cache madison docker-ce | awk -v v="5:${DOCKER_CE_VERSION}~" 'index($3, v) == 1 { print $3; exit }') \
    && containerd=$(apt-cache madison containerd.io | awk -v v="${CONTAINERD_IO_VERSION}~" 'index($3, v) == 1 { print $3; exit }') \
    && if [ -z "$ce" ] || [ -z "$containerd" ]; then \
        echo "Docker's $VERSION_CODENAME suite has no docker-ce $DOCKER_CE_VERSION or containerd.io $CONTAINERD_IO_VERSION" >&2; exit 1; \
    fi \
    && apt-get install -y --no-install-recommends \
        containerd.io="$containerd" \
        docker-buildx-plugin \
        docker-ce="$ce" \
        docker-ce-cli="$ce" \
        docker-compose-plugin \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl disable docker.service containerd.service \
    && systemctl enable docker.socket

# gh, from GitHub's own repository, which tracks its releases.
#
# GitHub's key is served beside the packages it signs, so it is held to the
# fingerprints GitHub publishes for it (cli/cli docs/install_linux.md). The
# keyring holds more than one primary key, so the pin is the whole set, sorted:
# a key missing from it, or one added to it, fails the build. 2C61...6059
# expired on 2026-09-05 and is still served; when GitHub drops it, or adds a
# key, this build fails until the pin and the cli feature's copy of the keyring
# (sandbox-agent/features/cli/githubcli-archive-keyring.asc) move with it.
ARG GH_APT_KEY_FINGERPRINTS="2C6106201985B60E6C7AC87323F3D4EA75716059 7F38BBB59D064DBCB3D84D725612B36462313325"
RUN set -eux \
    && curl -fsSL --retry 5 --retry-all-errors https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && gnupg=$(mktemp -d) \
    && fingerprints=$(GNUPGHOME=$gnupg gpg --show-keys --with-colons /etc/apt/keyrings/githubcli-archive-keyring.gpg | awk -F: '$1 == "pub" { primary = 1 } $1 == "fpr" && primary { print $10; primary = 0 }' | sort | paste -sd ' ') \
    && rm -rf "$gnupg" \
    && if [ "$fingerprints" != "$GH_APT_KEY_FINGERPRINTS" ]; then \
        echo "GitHub CLI's apt key is not $GH_APT_KEY_FINGERPRINTS: $fingerprints (compare GitHub's published fingerprints in cli/cli docs/install_linux.md)" >&2; exit 1; \
    fi \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && cat /etc/apt/mirrors/ubuntu.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends gh \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Go and uv, each its upstream release, pinned with its checksum: Ubuntu's Go
# trails upstream, and Ubuntu packages no uv. Go is the repository's one Go
# pin, the Go its builders use, moved by `go tool task go:bump`.
# Every curl in this image retries, any error included: one 504 from a release
# host would otherwise fail the build (#871). curl's own backoff (1s doubling,
# about 30s over five retries) rides out a short outage. curl -o discards a
# failed attempt's partial file before retrying, and the checksum checks what
# it kept.
ARG GO_VERSION=1.27.3
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
RUN set -eu \
    && arch="$(dpkg --print-architecture)" \
    && case "$arch" in \
        amd64) go_sha=$GO_SHA256_AMD64 ;; \
        arm64) go_sha=$GO_SHA256_ARM64 ;; \
        *) echo "no Go for $arch" >&2; exit 1 ;; \
    esac \
    && curl -fsSL --retry 5 --retry-all-errors -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" \
    && echo "$go_sha  /tmp/go.tar.gz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tar.gz \
    && rm /tmp/go.tar.gz \
    && /usr/local/go/bin/go version
ENV PATH=/usr/local/go/bin:${PATH}

ARG UV_VERSION=0.13.4
ARG UV_SHA256_AMD64=2b428396f12572d4429cf210ca234dd599a33b3af0958a4d5ed569d63e3f344f
ARG UV_SHA256_ARM64=1ae72b4718729e2ccc4f1ae2c6d1e531ca8467800e0546e9130b3886023a60ab
RUN set -eux \
    && arch="$(dpkg --print-architecture)" \
    && case "$arch" in \
        amd64) uv_arch=x86_64 uv_sha="$UV_SHA256_AMD64" ;; \
        arm64) uv_arch=aarch64 uv_sha="$UV_SHA256_ARM64" ;; \
        *) echo "unsupported architecture $arch" >&2; exit 1 ;; \
    esac \
    && tmp="$(mktemp -d)" \
    && curl -fsSL --retry 5 --retry-all-errors -o "$tmp/uv.tar.gz" "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uv_arch}-unknown-linux-gnu.tar.gz" \
    && echo "$uv_sha  $tmp/uv.tar.gz" | sha256sum -c - \
    && tar -C /usr/local/bin --strip-components=1 -xzf "$tmp/uv.tar.gz" "uv-${uv_arch}-unknown-linux-gnu/uv" "uv-${uv_arch}-unknown-linux-gnu/uvx" \
    && rm -rf "$tmp" \
    && uv --version

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
