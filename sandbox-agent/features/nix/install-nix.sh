#!/bin/bash
# Nix, installed to a local store and then moved aside as the seed (ADR 0075).
set -euo pipefail

seed=/usr/local/lib/discobox/nix
# Once the box is booted, /nix is the pool-shared cache (image.json): the
# store cannot be installed into it, nor moved off it, and the seed it was cut
# from is what that cache was seeded with. The step is applied where /nix is
# the box's own, as an image build or a snapshot provisions.
if mountpoint -q /nix; then
  echo "install-nix: /nix is a mount, the pool's shared store: Nix installs before the box boots" >&2
  exit 1
fi
# What an earlier run of this step left, whole or in part: a seed, which the
# store is moved onto and would nest inside, and an install, which the
# installer refuses to repeat.
rm -rf "$seed"
if [ -x /nix/nix-installer ]; then
  /nix/nix-installer uninstall --no-confirm
fi

unset NIX_REMOTE
# qemu-user (emulated cross-platform builds) cannot load seccomp BPF programs,
# so syscall filtering is off for the install alone, never in the box's own
# /etc/nix/nix.conf.
export NIX_CONFIG="filter-syscalls = false"
curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix \
  | sh -s -- install linux --extra-conf "sandbox = false" --init none --no-confirm
install -D -m 0644 /nix/var/nix/profiles/default/lib/systemd/system/nix-daemon.service /etc/systemd/system/nix-daemon.service
install -D -m 0644 /nix/var/nix/profiles/default/lib/systemd/system/nix-daemon.socket /etc/systemd/system/nix-daemon.socket
# The shipped unit points at a man page the box has no man pages for.
sed -i 's|^Documentation=man:nix-daemon https://nixos.org/manual$|Documentation=https://nixos.org/manual|' /etc/systemd/system/nix-daemon.service
/nix/var/nix/profiles/default/bin/nix profile install nixpkgs#devenv nixpkgs#bashInteractive
/nix/var/nix/profiles/default/bin/devenv --version

# ADR 0085: the stamp that guards the seed names the store it was cut from,
# and a pool cache that already holds another build's store merges this one
# in instead of overwriting the database that pool has been writing to. Both
# need facts only this step knows, recorded beside the seed rather than inside
# it: the store's identity -- a hash of its own content hashes, so an
# identical install re-seeds nobody -- the /nix path of the nix-store binary
# this store delivers, and a dump of the database rows that say which paths it
# registered.
install -d -m 0755 /usr/local/lib/discobox
{
  # shellcheck disable=SC2012 # Store paths are plain names, and the id is this listing's hash.
  echo "SEED_ID=$(LC_ALL=C ls -1 /nix/store | sha256sum | cut -d' ' -f1)"
  # The profile's store path plus the name, not `readlink -f` of the binary:
  # nix is one multi-call executable that dispatches on argv[0], so resolving
  # the link lands on `bin/nix` and loses the nix-store personality along
  # with `--load-db`.
  echo "SEED_NIX_STORE=$(readlink -f /nix/var/nix/profiles/default)/bin/nix-store"
} > /usr/local/lib/discobox/nix-seed.env
/nix/var/nix/profiles/default/bin/nix-store --dump-db > /usr/local/lib/discobox/nix-db.dump
# The store goes aside and /nix is left empty, for the cache volume to bind
# over. A rename, in the step that installed it: in an image build, moving it
# in a later layer would copy the whole store up into that layer.
mv /nix "$seed"
mkdir -m 0755 /nix
