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

# The tree is root:brew, group-writable, setgid so what is installed later
# keeps the group. Only what this step writes is given it, never the prefix
# whole: on a booted box the prefix is an overlay over the sandbox's data
# volume, where a recursive chown would copy every keg the sandbox installed
# up into it.
own() {
  for dir in "$@"; do
    [ -e "$dir" ] || continue
    chown -R root:brew "$dir"
    chmod -R g+w "$dir"
    find "$dir" -type d -exec chmod g+s {} +
  done
}
chown root:brew /home/linuxbrew "$prefix"
chmod g+ws /home/linuxbrew "$prefix"

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
own "$prefix/Homebrew" "$prefix/bin"
setpriv --reuid brew-build --regid brew --clear-groups \
  env HOME=/home/linuxbrew HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 \
  "$prefix/bin/brew" vendor-install ruby
setpriv --reuid brew-build --regid brew --clear-groups \
  env HOME=/home/linuxbrew "$prefix/bin/brew" --version
# The download cache vendor-install filled is scratch, and it landed beside
# the prefix because that is where HOME pointed.
rm -rf /home/linuxbrew/.cache
# Homebrew's own state, if running it made any; never the rest of var, where
# a formula's service keeps its data (a database refuses a group-writable
# directory).
if [ -d "$prefix/var" ]; then
  chown root:brew "$prefix/var"
fi
own "$prefix/Homebrew" "$prefix/bin" "$prefix/var/homebrew"
