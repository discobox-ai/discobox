# shellcheck shell=bash
# Sourced by what this repository runs in a box that would create Docker
# containers: the `.discobox/services` scripts and the Dockerfile test builds.
#
# docker_storage_unsupported prints why Docker can't create a container here and
# returns 0, or prints nothing and returns 1 when it can.
#
# An archil box keeps /var/lib/docker and /var/lib/containerd on the Archil data
# disk (`fuse.archil`). overlayfs needs an upper filesystem with tmpfile support,
# which that isn't, and the first container create takes the whole machine down
# with it (#1639). A caller that went ahead would crash the box (a service, every
# time the sandbox agent restarts Docker), so on such a box it exits instead.
# Remove this once #1639 is fixed.
#
# The path is checked, not dockerd: a service starts as Docker does, and asking
# a daemon that is still coming up would race it. A path that doesn't exist yet
# is judged by the filesystem it will be made on.

docker_storage_unsupported() {
	local dir at fstype
	for dir in /var/lib/docker /var/lib/containerd; do
		# Followed, in case it is a symlink into the data disk, and since findmnt
		# refuses a path that doesn't exist, asked about the nearest directory
		# that does.
		at=$(readlink -m "$dir")
		while [ ! -e "$at" ]; do at=$(dirname "$at"); done
		fstype=$(findmnt -no FSTYPE --target "$at" 2>/dev/null) || continue
		if [ "$fstype" = fuse.archil ]; then
			echo "$dir is on $fstype, where overlayfs can't run and creating a container crashes the machine (discobox-ai/discobox#1639)"
			return 0
		fi
	done
	return 1
}
