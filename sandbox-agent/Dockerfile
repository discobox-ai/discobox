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
COPY sandboxconfig ./sandboxconfig
COPY sandbox-agent ./sandbox-agent

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-sandbox-agent ./cmd/discobox-sandbox-agent \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-runc ./cmd/discobox-runc \
    && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-ca-anchor ./cmd/discobox-ca-anchor

FROM debian:bookworm AS sandbox-base

ARG DEBIAN_MIRROR
ARG DEBIAN_SECURITY_MIRROR

ENV container=docker
ENV DEBIAN_FRONTEND=noninteractive
ENV NIX_CONFIG="experimental-features = nix-command flakes"
ENV NIX_REMOTE=daemon
ENV PNPM_HOME=/var/lib/discobox/pnpm
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
    # Trim systemd down to what a container actually needs. The host owns the
    # kernel, hardware, partitions, and network namespace, so anything that
    # manages those is dead weight or an outright failure at boot. udev is
    # deliberately NOT masked: nested Docker in the sandbox depends on it.
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
COPY sandbox-agent/image/systemd/discobox-trust-ca.service /etc/systemd/system/discobox-trust-ca.service
COPY sandbox-agent/image/systemd/containerd.service.d/runc-ca.conf /etc/systemd/system/containerd.service.d/runc-ca.conf
COPY sandbox-agent/image/systemd/docker.service.d/proxy.conf /etc/systemd/system/docker.service.d/proxy.conf
COPY sandbox-agent/image/systemd/docker.service.d/proxy-bridge.conf /etc/systemd/system/docker.service.d/proxy-bridge.conf
COPY sandbox-agent/image/systemd/docker.service.d/runc-ca.conf /etc/systemd/system/docker.service.d/runc-ca.conf
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
# env injected into its OCI spec. See the sandbox-agent/runcca package.
COPY --from=sandbox-agent-builder /out/discobox-runc /opt/discobox/bin/runc
COPY --from=sandbox-agent-builder /out/discobox-ca-anchor /usr/local/bin/discobox-ca-anchor

RUN chmod 0755 /usr/local/bin/discobox-sandbox-agent \
    && chmod 0755 /opt/discobox/bin/runc \
    && chmod 0755 /usr/local/bin/discobox-ca-anchor \
    && chmod 0755 /usr/local/bin/discobox-vnc-websockify \
    && ln -sf /usr/local/bin/discobox-sandbox-agent /usr/local/bin/discobox-hook-publish \
    && mkdir -p /etc/discobox /run/discobox/agent-terminals /var/lib/docker /var/lib/discobox /workspace \
    && systemctl enable \
        discobox-sandbox-agent.service \
        discobox-proxy-bridge.service \
        discobox-trust-ca.service \
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
