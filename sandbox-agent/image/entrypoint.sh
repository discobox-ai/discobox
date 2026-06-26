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

  if ! [[ "$discobox_user_name" =~ ^[A-Za-z_][A-Za-z0-9_.-]*[$]?$ ]]; then
    echo "DISCOBOX_USER_NAME is not safe for sudoers: $discobox_user_name" >&2
    exit 1
  fi
  install -d -m 0750 /etc/sudoers.d
  printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$discobox_user_name" > /etc/sudoers.d/discobox-user
  chmod 0440 /etc/sudoers.d/discobox-user
  visudo -cf /etc/sudoers.d/discobox-user >/dev/null
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
if [ "$discobox_user_uid" = "0" ]; then
  chown --no-dereference "$discobox_user_uid:$discobox_user_gid" "$discobox_user_home"
else
  chown -R --no-dereference "$discobox_user_uid:$discobox_user_gid" "$discobox_user_home"
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
    if [ -f /etc/systemd/system/openbox@.service ] && [ -f /etc/systemd/system/xvfb.service ]; then
      install -d -m 0755 /etc/systemd/system/xvfb.service.d
      cat > /etc/systemd/system/xvfb.service.d/discobox-desktop-user.conf <<EOF
[Unit]
Wants=openbox@${discobox_user_name}.service
EOF
    fi
    if [ -f /etc/systemd/system/x11vnc@.service ]; then
      install -d -m 0755 /etc/systemd/system/x11vnc@.service.d
      cat > /etc/systemd/system/x11vnc@.service.d/discobox-desktop-user.conf <<EOF
[Service]
User=${discobox_user_name}
EOF
    fi
    if [ -f /etc/systemd/system/websockify@.service ] && [ -f /etc/systemd/system/websockify-proxy.service ]; then
      install -d -m 0755 /etc/systemd/system/websockify-proxy.service.d
      cat > /etc/systemd/system/websockify-proxy.service.d/discobox-desktop-user.conf <<EOF
[Unit]
Wants=websockify@${discobox_user_name}.service
After=websockify@${discobox_user_name}.service
EOF
    fi
    exec "$@"
    ;;
esac

if [ "$discobox_user_uid" = "0" ]; then
  exec env HOME="$discobox_user_home" USER="$discobox_user_name" LOGNAME="$discobox_user_name" "$@"
fi

exec runuser -u "$discobox_user_name" -- env HOME="$discobox_user_home" USER="$discobox_user_name" LOGNAME="$discobox_user_name" "$@"
