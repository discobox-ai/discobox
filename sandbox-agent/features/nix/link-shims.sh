#!/bin/sh
# nix, devenv and friends on /usr/local/bin, which PATH reaches only while the
# store's own bin directories are still empty: each starts the seed unit, then
# execs the real binary (ADR 0075). Linked by core's link-command, which moves
# aside what the box had at each name. With `check` (check-shims.sh) it
# changes nothing and fails when a link is gone or shadowed, so the next boot
# links it again.
set -eu

check=
if [ "${1-}" = check ]; then
  check=--check
fi
for c in nix nix-build nix-env nix-shell nix-store devenv; do
  /usr/local/libexec/discobox/link-command $check /usr/local/lib/discobox/nix-shim "$c"
done
