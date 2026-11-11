#!/bin/sh
# Pinned to a release tag: an unpinned clone is unreproducible, and the tag is
# load-bearing twice over -- brew derives its version from `git describe
# --tags`, and a shallow clone with no tag reachable reports "(shallow or no
# git repository)" instead of a version.
set -eu

version=6.0.22
prefix=/home/linuxbrew/.linuxbrew

rm -rf "$prefix/Homebrew"
git clone --depth 1 --branch "$version" https://github.com/Homebrew/brew "$prefix/Homebrew"
mkdir -p "$prefix/bin"
ln -sfn ../Homebrew/bin/brew "$prefix/bin/brew"
# git refuses to operate in a repository owned by another user, and every
# sandbox user is another user as far as this tree is concerned. The ownership
# split here is the design, not the tampering that check is looking for.
if ! git config --system --get-all safe.directory | grep -qxF "$prefix/Homebrew"; then
  git config --system --add safe.directory "$prefix/Homebrew"
fi

# Repair the shared installation before invoking brew. The prefix persists,
# while system groups can get different numbers on a later boot. Walk metadata
# but change only entries needing repair: an unconditional recursive chown or
# chmod copies every installed keg into an overlay's upper layer.
# Formula service data under var keeps its own ownership and permissions.
# Never follow a keg's symlink to something outside this shared installation.
mkdir -p "$prefix/var/homebrew"
repair() {
  find "$prefix" \( -path "$prefix/var/*" ! -path "$prefix/var/homebrew" \
    ! -path "$prefix/var/homebrew/*" \) -prune -o \
    ! -group brew -exec chgrp -h brew {} +
  find "$prefix" \( -path "$prefix/var/*" ! -path "$prefix/var/homebrew" \
    ! -path "$prefix/var/homebrew/*" \) -prune -o \
    ! -type l ! -perm -0020 -exec chmod g+w {} +
  find "$prefix" \( -path "$prefix/var/*" ! -path "$prefix/var/homebrew" \
    ! -path "$prefix/var/homebrew/*" \) -prune -o \
    -type d ! -perm -2000 -exec chmod g+s {} +
}
chown root:brew /home/linuxbrew "$prefix"
chmod g+ws /home/linuxbrew "$prefix"
repair

# Homebrew runs on its own vendored ruby and fetches it on first use. Left to
# first use, every sandbox would fetch it and copy it up into its own overlay;
# fetched here it is content they all share.
#
# A throwaway system account in the brew group runs it, and the ownership is
# reapplied afterwards over what it wrote. Not because brew refuses root, but
# because whether it does is the environment's answer to give: BuildKit's runc
# executor creates no /.dockerenv, and whether /proc/1/cgroup looks like a
# container depends on the driver. Running as a real user stops depending on
# it either way.
userdel brew-build 2>/dev/null || true
useradd --system --gid brew --no-create-home --home-dir /home/linuxbrew brew-build
trap 'userdel brew-build 2>/dev/null || true' EXIT
setpriv --reuid brew-build --regid brew --clear-groups \
  env HOME=/home/linuxbrew HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 \
  "$prefix/bin/brew" vendor-install ruby
setpriv --reuid brew-build --regid brew --clear-groups \
  env HOME=/home/linuxbrew "$prefix/bin/brew" --version
# The download cache vendor-install filled is scratch, and it landed beside
# the prefix because that is where HOME pointed.
rm -rf /home/linuxbrew/.cache
# Return Homebrew's own files to root before removing the throwaway account;
# installed kegs keep their owners. Do not follow symlinks out of these trees.
find "$prefix/Homebrew" "$prefix/var/homebrew" ! -user root -exec chown -h root {} +
# Normalize anything vendor-install created, including lock files. Stale lock
# files need no removal: Homebrew's advisory locks die with their process.
repair
