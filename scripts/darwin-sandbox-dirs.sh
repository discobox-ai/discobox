#!/usr/bin/env bash
# Give this Mac what a macOS sandbox's template gives its guest, so the
# sandbox agent's darwin boot test runs at the paths a pool's bootstrap names.
#
# macOS's sealed system volume has no /run, and no directory can be made at
# its root. A template makes /run (as it makes /workspace) through
# synthetic.conf, which links a root name onto the data volume; the agent's
# paths do not differ by platform. This adds the same entry, applies it
# without a reboot (apfs.util -t, as the Nix installer does for /nix), and
# makes the directories the agent's launch daemon would as root writable by
# the user the tests run as.
#
# It changes the machine, with sudo: ci:test:darwin runs it on every macOS
# runner. A step already in place is skipped, and sudo is used only for one
# that is not. Run from a terminal, sudo asks for the password; run without
# one, as a CI job does, it never asks and fails at once instead. Depot's
# disposable runners have passwordless sudo. A self-hosted Mac without it is
# prepared by running this by hand, as the user its jobs run as, once and
# again after each reboot, since macOS empties /var/run (where /run points) at
# boot.
#
# Usage: scripts/darwin-sandbox-dirs.sh

set -euo pipefail

if [ "$(uname -s)" != Darwin ]; then
	echo "darwin-sandbox-dirs: prepares a Mac" >&2
	exit 1
fi

# Without a terminal a password prompt can only hang or fail, so sudo is
# told never to ask, and need_root fails first, naming the fix.
sudo=(sudo)
if [ ! -t 0 ]; then
	sudo=(sudo -n)
fi
need_root() {
	if [ ! -t 0 ] && ! sudo -n true 2>/dev/null; then
		echo "darwin-sandbox-dirs: $1 needs root, and sudo here asks for a password." >&2
		echo "darwin-sandbox-dirs: run scripts/darwin-sandbox-dirs.sh by hand in a terminal on this Mac, as $(id -un), then run the job again." >&2
		exit 1
	fi
}

if [ ! -d /run ]; then
	need_root "making /run through /etc/synthetic.conf"
	if ! grep -qE '^run[[:space:]]' /etc/synthetic.conf 2>/dev/null; then
		printf 'run\tprivate/var/run\n' | "${sudo[@]}" tee -a /etc/synthetic.conf >/dev/null
	fi
	# It exits non-zero even when it has made every entry, so what it made is
	# checked below instead.
	"${sudo[@]}" /System/Library/Filesystems/apfs.fs/Contents/Resources/apfs.util -t || true
fi
if [ ! -d /run ]; then
	echo "darwin-sandbox-dirs: /run did not appear from /etc/synthetic.conf" >&2
	exit 1
fi

for dir in /run/discobox /var/lib/discobox; do
	if [ -d "$dir" ] && [ -O "$dir" ]; then
		continue
	fi
	need_root "making $dir"
	"${sudo[@]}" install -d -o "$(id -u)" -g "$(id -g)" -m 0755 "$dir"
done
