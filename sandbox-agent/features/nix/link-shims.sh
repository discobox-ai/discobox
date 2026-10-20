#!/bin/sh
# nix, devenv and friends on /usr/local/bin, which PATH reaches only while the
# store's own bin directories are still empty: each starts the seed unit, then
# execs the real binary (ADR 0075).
set -eu

for c in nix nix-build nix-env nix-shell nix-store devenv; do
  ln -sf /usr/local/lib/discobox/nix-shim "/usr/local/bin/$c"
done
