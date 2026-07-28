#!/bin/sh
# Build-once, exec-forever wrapper around bridge.c. Invoked as:
#   sh entrypoint.sh <target>
#
# On the first call per VM boot, no compiled bridge exists yet at
# $CACHE, so this builds one using the guest's own dockerd (the same
# daemon wslcsession talks to for everything else - no host-side C toolchain
# involved anywhere). $CACHE lives under /tmp, which is tmpfs and wiped on
# VM restart, so every fresh session pays this cost exactly once, on its
# first DockerConn/DialGuestUnix/DialGuestTCP call; every call after that
# just execs the cached binary directly.
set -e

DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
CACHE_DIR=/tmp/wslcsession-cache
CACHE="$CACHE_DIR/bridge"

if [ ! -x "$CACHE" ]; then
    mkdir -p "$CACHE_DIR"
    # alpine:latest, not a pinned version: this is a trivial, dependency-free
    # gcc+musl-dev static compile with no exposure to Alpine-version-specific
    # behavior, so there's nothing to gain from pinning and real cost to it -
    # Alpine cuts a new release every ~6 months and drops security support
    # for old ones after about 2 years (3.20, pinned here originally, was
    # already EOL by the time this was written). Floating avoids silently
    # building on an unsupported base again down the line.
    docker run --rm \
        -v "$DIR":/src:ro \
        -v "$CACHE_DIR":/out \
        alpine:latest sh -c \
        'apk add --no-cache gcc musl-dev >/dev/null 2>&1 && gcc -O2 -static -Wall -o /out/bridge /src/bridge.c && chmod 755 /out/bridge'
fi

exec "$CACHE" "$@"
