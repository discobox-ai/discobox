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

discobox_user_uid="${DISCOBOX_USER_UID:-0}"
discobox_user_gid="${DISCOBOX_USER_GID:-$discobox_user_uid}"
discobox_user_name="${DISCOBOX_USER_NAME:-root}"
discobox_user_home="${DISCOBOX_USER_HOME:-/root}"

if ! [[ "$discobox_user_uid" =~ ^[0-9]+$ ]] || ! [[ "$discobox_user_gid" =~ ^[0-9]+$ ]]; then
  echo "DISCOBOX_USER_UID and DISCOBOX_USER_GID must be numeric" >&2
  exit 1
fi

if [ "$discobox_user_uid" = "0" ]; then
  discobox_user_name="root"
  discobox_user_home="/root"
else
  if getent group "$discobox_user_gid" >/dev/null; then
    discobox_group_name="$(getent group "$discobox_user_gid" | cut -d: -f1)"
  elif getent group "$discobox_user_name" >/dev/null; then
    groupmod --gid "$discobox_user_gid" "$discobox_user_name"
    discobox_group_name="$discobox_user_name"
  else
    groupadd --gid "$discobox_user_gid" "$discobox_user_name"
    discobox_group_name="$discobox_user_name"
  fi

  if id -u "$discobox_user_name" >/dev/null 2>&1; then
    usermod --uid "$discobox_user_uid" --gid "$discobox_user_gid" --home "$discobox_user_home" "$discobox_user_name"
  elif getent passwd "$discobox_user_uid" >/dev/null; then
    existing_user="$(getent passwd "$discobox_user_uid" | cut -d: -f1)"
    if ! id -u "$discobox_user_name" >/dev/null 2>&1; then
      usermod --login "$discobox_user_name" "$existing_user"
    fi
    usermod --gid "$discobox_user_gid" --home "$discobox_user_home" "$discobox_user_name"
  else
    useradd --uid "$discobox_user_uid" --gid "$discobox_group_name" --home-dir "$discobox_user_home" --shell /bin/bash "$discobox_user_name"
  fi
fi

install -d -m 0755 -o "$discobox_user_uid" -g "$discobox_user_gid" "$discobox_user_home"
if [ -z "$(find "$discobox_user_home" -mindepth 1 -maxdepth 1 -print -quit)" ] && [ -d /etc/skel ]; then
  shopt -s dotglob nullglob
  skel_files=(/etc/skel/*)
  if [ "${#skel_files[@]}" -gt 0 ]; then
    cp -a "${skel_files[@]}" "$discobox_user_home"/
  fi
  shopt -u dotglob nullglob
fi
chown -R --no-dereference "$discobox_user_uid:$discobox_user_gid" "$discobox_user_home"

if [ "${DISCOBOX_SKIP_CODEX_UNIVERSAL_SETUP:-}" != "1" ] && [ -x /opt/codex/setup_universal.sh ]; then
  echo "Configuring codex-universal runtimes..."
  /opt/codex/setup_universal.sh
fi

if [ "$#" -eq 0 ]; then
  set -- sleep infinity
fi

case "$1" in
  bash|/bin/bash)
    shift
    set -- bash --login "$@"
    ;;
esac

if [ "$discobox_user_uid" = "0" ]; then
  exec env HOME="$discobox_user_home" USER="$discobox_user_name" LOGNAME="$discobox_user_name" "$@"
fi

exec runuser -u "$discobox_user_name" -- env HOME="$discobox_user_home" USER="$discobox_user_name" LOGNAME="$discobox_user_name" "$@"
EOF

WORKDIR /workspace
VOLUME ["/var/lib/docker", "/var/lib/discobox", "/workspace"]
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/opt/discobox/entrypoint.sh"]
CMD ["sleep", "infinity"]
