#!/bin/sh
# uv and uvx are still where install-uv.sh put them, and still what PATH finds.
set -eu
prefix=${DISCOBOX_UV_PREFIX:-/usr/local/lib/discobox/uv}
link_command=${DISCOBOX_LINK_COMMAND:-/usr/local/libexec/discobox/link-command}
for command in uv uvx; do
  "$link_command" --check "$prefix/$command"
done
