# syntax=docker/dockerfile:1.7

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
        systemd \
        systemd-sysv \
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

RUN cat >/etc/profile.d/sandbox-dev-tools.sh <<'EOF'
export PATH="/root/.nix-profile/bin:/nix/var/nix/profiles/default/bin:$PATH"

if [ -e /root/.nix-profile/etc/profile.d/nix.sh ]; then
  . /root/.nix-profile/etc/profile.d/nix.sh
fi
EOF

RUN mkdir -p /var/lib/docker /var/lib/discobox /workspace

RUN mkdir -p /opt/discobox \
    && cat >/opt/discobox/entrypoint.sh <<'EOF' \
    && chmod 0755 /opt/discobox/entrypoint.sh
#!/bin/bash
set -euo pipefail

if [ "${DISCOBOX_SKIP_CODEX_UNIVERSAL_SETUP:-}" != "1" ] && [ -x /opt/codex/setup_universal.sh ]; then
  echo "Configuring codex-universal runtimes..."
  /opt/codex/setup_universal.sh
fi

if [ "$#" -eq 0 ]; then
  set -- /sbin/init
fi

case "$1" in
  bash|/bin/bash)
    shift
    exec bash --login "$@"
    ;;
  *)
    exec "$@"
    ;;
esac
EOF

WORKDIR /workspace
VOLUME ["/var/lib/docker", "/var/lib/discobox", "/workspace"]
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/opt/discobox/entrypoint.sh"]
CMD ["/sbin/init"]
