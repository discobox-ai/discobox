# syntax=docker/dockerfile:1.7

FROM golang:1.26-bookworm AS sandbox-agent-builder

WORKDIR /src

COPY go.mod go.sum ./
COPY api ./api
COPY gormdb ./gormdb
COPY sandbox-agent ./sandbox-agent

RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go -C sandbox-agent build -trimpath -ldflags="-s -w" -o /out/discobox-sandbox-agent ./cmd/discobox-sandbox-agent

FROM ghcr.io/openai/codex-universal:latest

ENV container=docker
ENV PATH=/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}
ENV NIX_CONFIG="experimental-features = nix-command flakes"

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        dbus \
        iproute2 \
        less \
        procps \
        passwd \
        systemd \
        systemd-sysv \
        util-linux \
        vim \
        xfce4 \
        gdb \
        xfce4-terminal \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && . /etc/os-release \
    && printf 'Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/docker.asc\n' "$VERSION_CODENAME" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/docker.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        containerd.io \
        docker-buildx-plugin \
        docker-ce \
        docker-ce-cli \
        docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/* \
    && systemctl mask \
        dev-hugepages.mount \
        sys-fs-fuse-connections.mount \
        systemd-remount-fs.service \
        getty.target \
        console-getty.service \
    && systemctl enable docker.service

RUN curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix \
        | sh -s -- install linux --extra-conf "sandbox = false" --init none --no-confirm \
    && nix profile install nixpkgs#devenv nixpkgs#direnv nixpkgs#bashInteractive \
    && devenv --version \
    && direnv --version

COPY sandbox-agent/image/profile.d/sandbox-dev-tools.sh /etc/profile.d/sandbox-dev-tools.sh
COPY sandbox-agent/image/systemd/discobox-sandbox-agent.service /etc/systemd/system/discobox-sandbox-agent.service
COPY --from=sandbox-agent-builder /out/discobox-sandbox-agent /usr/local/bin/discobox-sandbox-agent

RUN chmod 0755 /usr/local/bin/discobox-sandbox-agent \
    && mkdir -p /etc/discobox /run/discobox/agent-terminals /var/lib/docker /var/lib/discobox /workspace \
    && systemctl enable discobox-sandbox-agent.service

COPY sandbox-agent/image/entrypoint.sh /opt/discobox/entrypoint.sh
RUN chmod 0755 /opt/discobox/entrypoint.sh

WORKDIR /workspace
VOLUME ["/var/lib/docker", "/var/lib/discobox", "/workspace"]
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/opt/discobox/entrypoint.sh"]
CMD ["/sbin/init"]
