#!/bin/bash
# uv, by its own installer, into a prefix Discobox owns, and uv and uvx linked
# onto the system PATH from there (link-command). Only when the box's own uv
# is not adopted, so a uv already at /usr/local/bin is one the adopt check
# refused: link-command moves it aside rather than leaving it to shadow ours.
# Installed into /usr/local/bin directly, the installer's mv would replace it
# silently instead (it renames over a link rather than writing through it).
#
# UV_NO_MODIFY_PATH: the prefix is on no PATH, and without it the installer
# writes it into root's shell profiles.
set -euo pipefail

prefix=${DISCOBOX_UV_PREFIX:-/usr/local/lib/discobox/uv}
link_command=${DISCOBOX_LINK_COMMAND:-/usr/local/libexec/discobox/link-command}

curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR="$prefix" UV_NO_MODIFY_PATH=1 sh
for command in uv uvx; do
  "$link_command" "$prefix/$command"
done
