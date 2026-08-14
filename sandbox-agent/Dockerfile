# syntax=docker/dockerfile:1.7

ARG DEBIAN_MIRROR=http://deb.debian.org/debian
ARG DEBIAN_SECURITY_MIRROR=http://security.debian.org/debian-security

# Build natively on the host platform and cross-compile, so multi-arch image
# builds only emulate the (apt-based) runtime stage, not the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS sandbox-agent-builder
ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
COPY api ./api
COPY execstream ./execstream
COPY gormdb ./gormdb
COPY harness ./harness
COPY id ./id
COPY proxy ./proxy
COPY runcca ./runcca
COPY sandboxconfig ./sandboxconfig
COPY sandboxuser ./sandboxuser
COPY sandbox-agent ./sandbox-agent

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-sandbox-agent ./cmd/discobox-sandbox-agent \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-runc ./cmd/discobox-runc \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-ca-anchor ./cmd/discobox-ca-anchor \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-docker ./cmd/discobox-docker

# Built in its own layer so bumping the difftui version does not invalidate the
# sandbox-agent binaries above.
# Pinned to a main pseudo-version rather than `latest`: difftui carries no
# tags, so `latest` is main's tip anyway, but an unpinned version makes the
# build unreproducible and leaves this layer cached on a stale commit. Bump it
# to pull a newer main.
ARG DIFFTUI_VERSION=v0.0.0-20260808165440-27da2073239f
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    set -eux \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
        go install -trimpath -ldflags="-s -w" github.com/discobox-ai/difftui@${DIFFTUI_VERSION} \
    # `go install` refuses a GOBIN with cross-compilation and drops
    # cross-built binaries under GOPATH/bin/<goos>_<goarch> instead, so pick
    # the binary up from whichever of the two layouts was used.
    && mv "$(go env GOPATH)/bin/${TARGETOS}_${TARGETARCH}/difftui" /out/difftui \
        || mv "$(go env GOPATH)/bin/difftui" /out/difftui

FROM debian:bookworm AS sandbox-base

ARG DEBIAN_MIRROR
ARG DEBIAN_SECURITY_MIRROR

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive
ENV NIX_CONFIG="experimental-features = nix-command flakes"
ENV NIX_REMOTE=daemon
ENV PATH=/nix/var/nix/profiles/default/bin:/usr/local/go/bin:/usr/local/bin:${PATH}

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    set -eux \
    && sed -i \
        -e "s|http://deb.debian.org/debian|${DEBIAN_MIRROR}|g" \
        -e "s|http://security.debian.org/debian-security|${DEBIAN_SECURITY_MIRROR}|g" \
        /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        chromium \
        curl \
        dbus \
        direnv \
        extrepo \
        git \
        gnupg \
        iptables \
        iproute2 \
        jq \
        less \
        libnss3-tools \
        openssh-client \
        openssh-sftp-server \
        passwd \
        poppler-utils \
        procps \
        psmisc \
        python-is-python3 \
        python3 \
        python3-pip \
        python3-requests \
        python3-venv \
        python3-websockify \
        python3-xdg \
        ripgrep \
        scrot \
        shellcheck \
        socat \
        sqlite3 \
        sudo \
        systemd \
        systemd-sysv \
        unzip \
        util-linux \
        vim \
        x11vnc \
        x11-xserver-utils \
        xdotool \
        xserver-xorg-core \
        xserver-xorg-video-dummy \
        xterm \
        xvfb \
        openbox \
        pcmanfm \
        xz-utils \
    && curl -fsSL https://deb.nodesource.com/setup_25.x | bash - \
    && sed -i 's|http://|https://|g' /etc/apt/sources.list.d/nodesource.list 2>/dev/null || true \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$(grep VERSION_CODENAME /etc/os-release | cut -d= -f2)" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/docker.sources \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && extrepo enable mise \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        containerd.io \
        docker-buildx-plugin \
        docker-ce \
        docker-ce-cli \
        docker-compose-plugin \
        gh \
        mise \
        nodejs \
    && curl -fsSL https://code-server.dev/install.sh | sh \
    && mkdir -p /opt/discobox/code-server-defaults/extensions \
    && code-server --install-extension vscodevim.vim --extensions-dir /opt/discobox/code-server-defaults/extensions \
    && code-server --install-extension golang.go --extensions-dir /opt/discobox/code-server-defaults/extensions \
    && code-server --install-extension rust-lang.rust-analyzer --extensions-dir /opt/discobox/code-server-defaults/extensions \
    && code-server --install-extension ms-python.python --extensions-dir /opt/discobox/code-server-defaults/extensions \
    && rm -f /opt/discobox/code-server-defaults/extensions/extensions.json \
    && npm install -g pnpm \
    && GO_VERSION="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)" \
    && curl -fsSL "https://go.dev/dl/${GO_VERSION}.linux-$(dpkg --print-architecture).tar.gz" | tar -C /usr/local -xz \
    && curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh \
    && curl -fsSL https://bun.sh/install | BUN_INSTALL=/usr/local bash \
    && rm -rf /var/lib/apt/lists/* /root/.npm \
    && rm -f /etc/apt/apt.conf.d/docker-clean \
    # Build the complete system trust store into the image, so the boot path
    # only has to add this sandbox's MITM CA to it rather than rebuilding all
    # 152 certificates to add one (see runcca.MaterializeTrustStore).
    #
    # It is built at /opt rather than /etc/ssl/certs, and by a redirected copy
    # of update-ca-certificates rather than the tool itself, because a build
    # running inside a Discobox sandbox has /etc/ssl/certs bind-mounted over by
    # the runc wrapper: writes there go to the mount and vanish when the layer
    # commits, which is why the image used to ship an empty store. LOCALCERTSDIR
    # is pointed away so no build-time anchor is baked in; anchors belong to the
    # sandbox that boots, and it adds its own.
    && mkdir -p /opt/discobox/ca-certificates \
    && sed -e 's|^ETCCERTSDIR=.*|ETCCERTSDIR=/opt/discobox/ca-certificates|' \
           -e 's|^LOCALCERTSDIR=.*|LOCALCERTSDIR=/nonexistent|' \
           /usr/sbin/update-ca-certificates > /tmp/discobox-prebuild-ca-store \
    && chmod +x /tmp/discobox-prebuild-ca-store \
    && /tmp/discobox-prebuild-ca-store --fresh \
    && rm -f /tmp/discobox-prebuild-ca-store \
    && test -s /opt/discobox/ca-certificates/ca-certificates.crt \
    # Trim systemd down to what a container actually needs. The host owns the
    # kernel, hardware, partitions, and network namespace, so anything that
    # manages those is dead weight or an outright failure at boot.
    #
    # udev is masked here too, which earlier revisions of this file said it
    # could not be: "nested Docker in the sandbox depends on it". Nothing does.
    # A sandbox with the data volumes wired (the harness image.json declares
    # /var/lib/docker and /var/lib/containerd on `data`, without which nested
    # Docker fails on overlay-on-overlay for reasons unrelated to udev) runs
    # `docker run`, `docker build`, and HTTPS from inside a nested container
    # with udev fully masked, reaching is-system-running=running with zero
    # failed units.
    #
    # The cost it was imposing: systemd-udev-trigger replays a uevent for every
    # device already in /sys, which in a container is the host's hardware — 786
    # of them here — and it is ordered Before=sysinit.target, so the sandbox
    # agent and its primary terminal wait behind the whole enumeration. What it
    # bought, measured by diffing /dev across a boot with and without it, was
    # six symlink trees for host devices: block, char, disk, ptp_hyperv, rtc,
    # virtio-ports. The container's /dev is a Docker-provided tmpfs rather than
    # devtmpfs, and nested Docker runs on overlayfs, so none of them had a
    # consumer. Userspace boot went 2.08s -> 237ms.
    # Periodic maintenance (apt, dpkg, fstrim, e2scrub) is masked so sandboxes
    # never wake up and mutate packages or filesystems behind the user.
    && systemctl mask \
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
        systemd-volatile-root.service \
    && systemctl disable docker.service containerd.service \
    && systemctl enable docker.socket

RUN set -eux; \
    unset NIX_REMOTE; \
    # Build-time only: qemu-user (emulated cross-platform builds) cannot load
    # seccomp BPF programs, so disable nix syscall filtering for the install
    # steps without persisting it into the image's /etc/nix/nix.conf.
    export NIX_CONFIG="filter-syscalls = false"; \
    curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix \
        | sh -s -- install linux --extra-conf "sandbox = false" --init none --no-confirm; \
    install -D -m 0644 /nix/var/nix/profiles/default/lib/systemd/system/nix-daemon.service /etc/systemd/system/nix-daemon.service; \
    install -D -m 0644 /nix/var/nix/profiles/default/lib/systemd/system/nix-daemon.socket /etc/systemd/system/nix-daemon.socket; \
    systemctl enable nix-daemon.socket; \
    nix profile install nixpkgs#devenv nixpkgs#bashInteractive; \
    devenv --version

RUN sed -i 's|^Documentation=man:nix-daemon https://nixos.org/manual$|Documentation=https://nixos.org/manual|' /etc/systemd/system/nix-daemon.service

RUN rustup --version >/dev/null 2>&1 \
    || curl --proto "=https" --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain none

COPY sandbox-agent/image/profile.d/sandbox-dev-tools.sh /etc/profile.d/sandbox-dev-tools.sh
COPY sandbox-agent/image/systemd/discobox-sandbox-agent.service /etc/systemd/system/discobox-sandbox-agent.service
COPY sandbox-agent/image/systemd/discobox-proxy-bridge.service /etc/systemd/system/discobox-proxy-bridge.service
COPY sandbox-agent/image/systemd/discobox-buildkit-bridge.service /etc/systemd/system/discobox-buildkit-bridge.service
COPY sandbox-agent/image/systemd/discobox-trust-ca.service /etc/systemd/system/discobox-trust-ca.service
COPY sandbox-agent/image/systemd/discobox-render-proxy-env.service /etc/systemd/system/discobox-render-proxy-env.service
COPY sandbox-agent/image/systemd/containerd.service.d/runc-ca.conf /etc/systemd/system/containerd.service.d/runc-ca.conf
COPY sandbox-agent/image/systemd/docker.service.d/runc-ca.conf /etc/systemd/system/docker.service.d/runc-ca.conf
COPY sandbox-agent/image/systemd/docker.service.d/proxy.conf /etc/systemd/system/docker.service.d/proxy.conf
COPY sandbox-agent/image/systemd/docker.service.d/proxy-bridge.conf /etc/systemd/system/docker.service.d/proxy-bridge.conf
COPY sandbox-agent/image/systemd/discobox-proxy-bridge-docker.service /etc/systemd/system/discobox-proxy-bridge-docker.service
COPY sandbox-agent/image/daemon.json /etc/docker/daemon.json
COPY sandbox-agent/image/systemd/journald-console.conf /etc/systemd/journald.conf.d/discobox-console.conf
COPY sandbox-agent/image/systemd/x11-display.socket /etc/systemd/system/x11-display.socket
COPY sandbox-agent/image/systemd/x11-display.service /etc/systemd/system/x11-display.service
COPY sandbox-agent/image/systemd/x11vnc.socket /etc/systemd/system/x11vnc.socket
COPY sandbox-agent/image/systemd/x11vnc@.service /etc/systemd/system/x11vnc@.service
COPY sandbox-agent/image/systemd/websockify-proxy.socket /etc/systemd/system/websockify-proxy.socket
COPY sandbox-agent/image/systemd/websockify-proxy.service /etc/systemd/system/websockify-proxy.service
COPY sandbox-agent/image/systemd/websockify@.service /etc/systemd/system/websockify@.service
COPY sandbox-agent/image/systemd/xvfb.service /etc/systemd/system/xvfb.service
COPY sandbox-agent/image/systemd/openbox@.service /etc/systemd/system/openbox@.service
COPY sandbox-agent/image/xorg-dummy.conf /etc/X11/xorg-dummy.conf
COPY sandbox-agent/image/discobox-vnc-websockify /usr/local/bin/discobox-vnc-websockify
COPY --from=sandbox-agent-builder /out/discobox-sandbox-agent /usr/local/bin/discobox-sandbox-agent
# Installs as `runc` on a directory prepended to containerd's and dockerd's
# PATH, so every container either one starts has the MITM CA bundles and proxy
# env injected into its OCI spec. See the shared runcca package.
COPY --from=sandbox-agent-builder /out/discobox-runc /opt/discobox/bin/runc
COPY --from=sandbox-agent-builder /out/discobox-ca-anchor /usr/local/bin/discobox-ca-anchor
# Installs as `docker`, ahead of the real CLI at /usr/bin/docker on PATH, to
# give `docker build` the pool-shared BuildKit cache. Non-build commands are
# exec'd straight through. See the sandbox-agent/dockercache package.
COPY --from=sandbox-agent-builder /out/discobox-docker /usr/local/bin/docker
COPY --from=sandbox-agent-builder /out/difftui /usr/local/bin/difftui

RUN chmod 0755 /usr/local/bin/discobox-sandbox-agent \
    && chmod 0755 /opt/discobox/bin/runc \
    && chmod 0755 /usr/local/bin/discobox-ca-anchor \
    && chmod 0755 /usr/local/bin/docker \
    && chmod 0755 /usr/local/bin/difftui \
    && chmod 0755 /usr/local/bin/discobox-vnc-websockify \
    && ln -sf /usr/local/bin/discobox-sandbox-agent /usr/local/bin/discobox-hook-publish \
    && mkdir -p /etc/discobox /run/discobox/agent-terminals /var/lib/docker /var/lib/discobox /workspace \
    && systemctl enable \
        discobox-sandbox-agent.service \
        discobox-proxy-bridge.service \
        discobox-buildkit-bridge.service \
        discobox-trust-ca.service \
        discobox-render-proxy-env.service \
        x11-display.socket \
        x11vnc.socket \
        websockify-proxy.socket

WORKDIR /workspace
# No VOLUME directives: the sandbox-agent init flow (PID 1) wires all persistent
# and cached paths from the primary volumes the worker mounts, per ADR 0007.
# Anonymous volumes here would only shadow those binds.
EXPOSE 6080
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/discobox-sandbox-agent", "init"]
CMD ["/sbin/init"]

FROM sandbox-base AS sandbox-agent

# Every harness image is built FROM this one and inherits the label through the
# image config, which is also how it survives a pull — that inheritance is why no
# harness Dockerfile repeats it. See harness.ReclaimLabel and ADR 0040.
LABEL io.discobox.reclaimable.v1=true
