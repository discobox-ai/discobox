#!/usr/bin/env bash
# Build watcher executables without ever writing over a live binary.
set -euo pipefail

mode=${1:?usage: dev-build.sh cli|server}
case "$mode" in
cli | server) ;;
*) echo "unknown build mode: $mode" >&2; exit 2 ;;
esac

mkdir -p build
stage=$(mktemp -d build/.dev-build.XXXXXX)
retired=
live=
cleanup() {
	if [ -n "$retired" ] && [ -f "$retired" ] && [ ! -e "$live" ]; then
		mv -- "$retired" "$live"
	fi
	rm -rf -- "$stage"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
exe=$(go env GOEXE)

# Every output starts absent, so Go cannot trust a damaged target's build ID.
(cd cli && go build -o "../$stage/discobox$exe" ./cmd/discobox)
if [ "$mode" = server ]; then
	(cd server && go build -gcflags="all=-N -l" -o "../$stage/discobox-server$exe" ./cmd/discobox-server)
	# Signing must finish before publication (a no-op off macOS).
	go tool task sign "SIGN_BINARY=$stage/discobox-server$exe"
fi

# Windows cannot replace a running executable. Keep it beside the live path
# until it exits; unique backup names also let the two watchers publish safely.
# Old mapped images may resist deletion until a later build.
if [ "$exe" = .exe ]; then
	for old in build/.dev-retiring-*.old; do
		if [ -f "$old" ]; then
			rm -f -- "$old" 2>/dev/null || true
		fi
	done
fi
publish() {
	local name=$1
	retired=
	live="build/$name"
	if [ "$exe" = .exe ] && [ -f "build/$name" ]; then
		retired=$(mktemp build/.dev-retiring-XXXXXX)
		# Remove the placeholder: Windows cannot replace even an empty file
		# with a mapped executable on every filesystem.
		rm -f -- "$retired"
		mv -- "build/$name" "$retired"
	fi
	if mv -f -- "$stage/$name" "build/$name"; then
		if [ -n "$retired" ]; then
			# Only completed publications become eligible for later cleanup.
			rm -f -- "$retired" 2>/dev/null || mv -- "$retired" "$retired.old"
		fi
	else
		# Another watcher may already have installed a complete replacement.
		if [ -n "$retired" ] && [ ! -e "$live" ]; then
			mv -- "$retired" "$live"
		fi
		return 1
	fi
}

# POSIX uses a single same-filesystem rename; Windows needs two renames when
# replacing a running binary. Neither path copies bytes into the live output.
publish "discobox$exe"
if [ "$mode" = server ]; then
	publish "discobox-server$exe"
fi
