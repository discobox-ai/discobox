#!/bin/sh
# voxl, the desktop's stream: its binary and the browser client and decode
# worker built from the same source, which the build put beside this script,
# linked rather than copied so a new tree brings its own. The viewer starts the
# binary and serves web/ as /voxl/ (`discobox-sandbox-agent desktop`).
#
# With `check` (check-voxl.sh) it changes nothing and fails when the link no
# longer points at this tree, so the next boot links it again.
set -eu

link=${DISCOBOX_VOXL_LINK:-/usr/local/lib/discobox/voxl}
target=${DISCOBOX_PROVISION_FEATURE_DIR:?}/voxl

if [ "${1-}" = check ]; then
  [ "$(readlink "$link" 2>/dev/null)" = "$target" ] && [ -x "$target/voxl" ]
  exit
fi
install -d -m 0755 "$(dirname "$link")"
ln -sfn "$target" "$link"
