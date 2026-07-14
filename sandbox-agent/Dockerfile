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
COPY gormdb ./gormdb
COPY harness ./harness
COPY id ./id
COPY proxy ./proxy
COPY sandbox-agent ./sandbox-agent

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-sandbox-agent ./cmd/discobox-sandbox-agent

FROM debian:bookworm

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
    && systemctl mask \
        dev-hugepages.mount \
        sys-fs-fuse-connections.mount \
        systemd-remount-fs.service \
        getty.target \
        console-getty.service \
        systemd-logind.service \
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
COPY sandbox-agent/image/image.json /usr/share/discobox/image.json
COPY sandbox-agent/image/systemd/discobox-sandbox-agent.service /etc/systemd/system/discobox-sandbox-agent.service
COPY sandbox-agent/image/systemd/discobox-proxy-bridge.service /etc/systemd/system/discobox-proxy-bridge.service
COPY sandbox-agent/image/systemd/discobox-trust-ca.service /etc/systemd/system/discobox-trust-ca.service
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

RUN chmod 0755 /usr/local/bin/discobox-sandbox-agent \
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

COPY sandbox-agent/image/entrypoint.sh /opt/discobox/entrypoint.sh
RUN chmod 0755 /opt/discobox/entrypoint.sh

WORKDIR /workspace
VOLUME ["/var/lib/docker", "/var/lib/discobox", "/workspace"]
EXPOSE 6080
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/opt/discobox/entrypoint.sh"]
CMD ["/sbin/init"]
