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
# It changes the machine, with sudo: ci:test:darwin runs it on a disposable
# CI runner.
#
# Usage: scripts/darwin-sandbox-dirs.sh

set -euo pipefail

if [ "$(uname -s)" != Darwin ]; then
	echo "darwin-sandbox-dirs: prepares a Mac" >&2
	exit 1
fi

if [ ! -d /run ]; then
	if ! grep -qE '^run[[:space:]]' /etc/synthetic.conf 2>/dev/null; then
		printf 'run\tprivate/var/run\n' | sudo tee -a /etc/synthetic.conf >/dev/null
	fi
	# It exits non-zero even when it has made every entry, so what it made is
	# checked below instead.
	sudo /System/Library/Filesystems/apfs.fs/Contents/Resources/apfs.util -t || true
fi
if [ ! -d /run ]; then
	echo "darwin-sandbox-dirs: /run did not appear from /etc/synthetic.conf" >&2
	exit 1
fi

sudo install -d -o "$(id -u)" -g "$(id -g)" -m 0755 /run/discobox /var/lib/discobox
