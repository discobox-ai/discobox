#!/bin/sh
# rustup and its proxies are still where install-rustup.sh put them, and still
# what PATH finds.
set -eu
cargo_home=${DISCOBOX_RUST_PREFIX:-/usr/local/lib/discobox/rust}
link_command=${DISCOBOX_LINK_COMMAND:-/usr/local/libexec/discobox/link-command}
[ -x "$cargo_home/bin/rustup" ]
for command in "$cargo_home"/bin/*; do
  "$link_command" --check "$command"
done
