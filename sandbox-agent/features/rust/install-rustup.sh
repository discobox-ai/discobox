#!/bin/bash
# rustup, with no toolchain, for every user of the box.
#
# rustup's installer puts rustup and its proxies (cargo, rustc, rustfmt, ...)
# in CARGO_HOME/bin, by default the installing user's ~/.cargo — root's here,
# which no sandbox user can read. So CARGO_HOME is a prefix Discobox owns, the
# installer is told to leave every shell profile alone, and each proxy is
# linked onto the system PATH from there (link-command). Only the install is
# shared: at run time the proxies take RUSTUP_HOME and CARGO_HOME from the
# user's own defaults, so each user's toolchains land in their own ~/.rustup,
# and the registry and `cargo install` in their own ~/.cargo, both declared
# volumes. The install's own RUSTUP_HOME is scratch, removed after.
set -euo pipefail

cargo_home=${DISCOBOX_RUST_PREFIX:-/usr/local/lib/discobox/rust}
link_command=${DISCOBOX_LINK_COMMAND:-/usr/local/libexec/discobox/link-command}

# Tested by its path, never on PATH: a rustup there may be one user's.
if [ ! -x "$cargo_home/bin/rustup" ]; then
  rustup_home=$(mktemp -d)
  trap 'rm -rf "$rustup_home"' EXIT
  curl --proto "=https" --tlsv1.2 -sSf https://sh.rustup.rs |
    env CARGO_HOME="$cargo_home" RUSTUP_HOME="$rustup_home" sh -s -- -y --no-modify-path --default-toolchain none
fi
for command in "$cargo_home"/bin/*; do
  "$link_command" "$command"
done
