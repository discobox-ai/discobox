#!/bin/sh
# Run the image reaper's Docker integration tests (pool-agent/imagereap) against
# a throwaway daemon of each image store, the classic store (overlay2) and the
# containerd store.
#
# Usage: scripts/image-reclaim-integration.sh [go test flags...]
#
# The two stores record an image's arrival differently: the classic store sets
# LastTagTime only on a tag, never on a load or a pull, and every shipped VM pool
# runs it (ADR 26-11-13-655). The reaper's handling of that is only exercised on
# a classic-store daemon, which neither CI's Docker nor a developer's usually
# is, so each store gets its own docker:dind container here. They are removed
# on exit, with everything the tests left in them.
#
# Needs Docker able to run a privileged container. $DIND_IMAGE names the image
# (default docker:29-dind).
set -eu

dind_image=${DIND_IMAGE:-docker:29-dind}
root=$(cd "$(dirname "$0")/.." && pwd)
# A Unix socket path is limited to about 100 bytes, so the sockets live under a
# short temporary directory rather than the checkout.
sockets=$(mktemp -d /tmp/discobox-reap.XXXXXX)
containers=

# Run only by the trap below: shellcheck 0.10 calls its body unreachable
# (SC2317), and 0.11 the function never invoked (SC2329).
# shellcheck disable=SC2317,SC2329
cleanup() {
	for name in $containers; do
		docker rm -f -v "$name" >/dev/null 2>&1 || true
	done
	rm -rf "$sockets"
}
trap cleanup EXIT INT TERM

# start_daemon NAME [dockerd flags...] starts a dind daemon whose socket is
# $sockets/NAME/docker.sock, and waits until it answers.
start_daemon() {
	name=$1
	shift
	container=discobox-reap-$name-$$
	mkdir -p "$sockets/$name"
	containers="$containers $container"
	docker run -d --privileged --name "$container" \
		-v "$sockets/$name:/run/discobox-reap" \
		"$dind_image" dockerd --host=unix:///run/discobox-reap/docker.sock "$@" >/dev/null
	tries=0
	until docker exec "$container" docker -H unix:///run/discobox-reap/docker.sock version >/dev/null 2>&1; do
		tries=$((tries + 1))
		if [ "$tries" -ge 120 ]; then
			echo "image-reclaim-integration: the $name daemon did not start" >&2
			docker logs "$container" >&2 || true
			exit 1
		fi
		sleep 1
	done
	# The daemon runs as root in its container, and the tests as whoever runs
	# this script.
	docker exec "$container" chmod 666 /run/discobox-reap/docker.sock
}

start_daemon classic --storage-driver overlay2 --feature containerd-snapshotter=false
start_daemon containerd --feature containerd-snapshotter=true

status=0
for store in classic containerd; do
	echo "--- image reaper integration tests, $store image store"
	if ! (cd "$root/pool-agent" &&
		DOCKER_HOST="unix://$sockets/$store/docker.sock" DISCOBOX_DOCKER_INTEGRATION=1 \
			go test ./imagereap -run Integration -count=1 -v "$@"); then
		status=1
	fi
done
exit "$status"
