#!/bin/bash
# Bun, by its own installer, into a prefix Discobox owns, and bun and bunx
# linked onto the system PATH from there (link-command), which moves aside
# what the box had at each name -- a bun an earlier provisioning installed
# straight into /usr/local/bin among it.
#
# HOME and SHELL: the installer adds its directory to the invoking user's
# shell profile, which would be root's; the prefix is on no PATH, so it is
# given a home of its own that is thrown away, and a shell it edits nothing
# for.
set -euo pipefail

prefix=${DISCOBOX_BUN_PREFIX:-/usr/local/lib/discobox/bun}
link_command=${DISCOBOX_LINK_COMMAND:-/usr/local/libexec/discobox/link-command}

home=$(mktemp -d)
trap 'rm -rf "$home"' EXIT
curl -fsSL https://bun.sh/install | env HOME="$home" SHELL=/bin/sh BUN_INSTALL="$prefix" bash
# The installer lays no bunx: bun is bunx when run by that name.
for command in bun bunx; do
  "$link_command" "$prefix/bin/bun" "$command"
done
