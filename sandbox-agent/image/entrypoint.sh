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

case "$1" in
  /sbin/init|/lib/systemd/systemd|systemd)
    exec "$@"
    ;;
esac

if [ "$discobox_user_uid" = "0" ]; then
  exec env HOME="$discobox_user_home" USER="$discobox_user_name" LOGNAME="$discobox_user_name" "$@"
fi

exec runuser -u "$discobox_user_name" -- env HOME="$discobox_user_home" USER="$discobox_user_name" LOGNAME="$discobox_user_name" "$@"
