#!/usr/bin/env bash
# Local build artifact for the spike; no distributed binary is produced.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
source_dir="$root/.tmp/voxl-source"
revision=9d004076f73cb374bdb87ff96d2e23e44b9f47b9
if [ ! -d "$source_dir/.git" ]; then
  mkdir -p "$root/.tmp"
  git clone https://github.com/tembo/voxl.git "$source_dir"
fi
if [ -n "$(git -C "$source_dir" status --porcelain)" ]; then
  echo "voxl source has local changes; preserve them before rebuilding" >&2
  exit 1
fi
git -C "$source_dir" checkout --detach "$revision"
export VOXL_SPIKE_ROOT="$root"
(cd "$source_dir" && nix-shell shell.nix --arg pkgs 'import (builtins.getFlake (builtins.getEnv "VOXL_SPIKE_ROOT")).inputs.nixpkgs {}' --arg withRust true --run 'cargo build --release --locked')
bash "$root/test/spikes/voxl/pack-runtime.sh" "$source_dir/target/release/voxl" "$root/.tmp/voxl-runtime"
printf 'Built %s locally in %s\n' "$revision" "$root/.tmp/voxl-runtime"
