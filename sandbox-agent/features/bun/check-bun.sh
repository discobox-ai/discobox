#!/bin/sh
# bun and bunx are still where install-bun.sh put them, and still what PATH
# finds.
set -eu
prefix=${DISCOBOX_BUN_PREFIX:-/usr/local/lib/discobox/bun}
link_command=${DISCOBOX_LINK_COMMAND:-/usr/local/libexec/discobox/link-command}
for command in bun bunx; do
  "$link_command" --check "$prefix/bin/bun" "$command"
done
