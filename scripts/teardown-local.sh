#!/bin/sh
# Return this machine to the state it was in before Discobox ever ran here.
#
# Stops the server, removes every container, image and volume the Docker daemon
# holds, and deletes Discobox's directories. It is meant for a dedicated
# development sandbox and it is not selective: the Docker half removes
# everything on the daemon, not only Discobox's.
#
# It refuses to do any of that without --yes, and prints what it would remove
# instead — which is also the quickest way to see what is currently lying around.
set -eu

DESTROY=0
for arg in "$@"; do
    case "$arg" in
        -y | --yes) DESTROY=1 ;;
        -h | --help)
            echo "usage: scripts/teardown-local.sh [--yes]"
            echo
            echo "  no flag   list what would be removed"
            echo "  --yes     remove it"
            echo
            echo "This RESETS DOCKER COMPLETELY: every container, image, volume"
            echo "and build cache on the daemon, whether or not it is Discobox's."
            echo "It is for a dedicated development machine."
            exit 0
            ;;
        *)
            echo "teardown-local: unknown argument $arg" >&2
            exit 2
            ;;
    esac
done

# Directories are read from the environment first, so a run configured through
# .env or an exported override is torn down where it actually lives rather than
# where the defaults say it should. The server reads .env itself, so this reads
# it too; nothing else here would know about it.
if [ -f .env ]; then
    # shellcheck disable=SC1091 # .env is the developer's own, and may not exist.
    . ./.env
fi

home_default() {
    if [ -n "${1:-}" ]; then
        printf %s "$1"
    else
        printf %s "$2"
    fi
}

# The server resolves these through adrg/xdg, which is not XDG on macOS: it puts
# data, config and state together under ~/Library/Application Support and cache
# under ~/Library/Caches. Assuming the Linux layout everywhere is not a cosmetic
# mistake — on a Mac it names four directories that do not exist, reports them
# absent, deletes nothing, and leaves the state it claimed to have removed.
case "$(uname -s)" in
    Darwin)
        DEFAULT_DATA="$HOME/Library/Application Support/discobox"
        DEFAULT_CONFIG="$DEFAULT_DATA"
        DEFAULT_STATE="$DEFAULT_DATA"
        DEFAULT_CACHE="$HOME/Library/Caches/discobox"
        ;;
    *)
        DEFAULT_DATA="${XDG_DATA_HOME:-$HOME/.local/share}/discobox"
        DEFAULT_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/discobox"
        DEFAULT_STATE="${XDG_STATE_HOME:-$HOME/.local/state}/discobox"
        DEFAULT_CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/discobox"
        ;;
esac

DATA_DIR=$(home_default "${DISCOBOX_DATA_DIR:-}" "$DEFAULT_DATA")
CONFIG_DIR=$(home_default "${DISCOBOX_CONFIG_DIR:-}" "$DEFAULT_CONFIG")
CACHE_DIR=$(home_default "${DISCOBOX_CACHE_DIR:-}" "$DEFAULT_CACHE")
STATE_DIR=$(home_default "${DISCOBOX_STATE_DIR:-}" "$DEFAULT_STATE")
# The socket lives in a runtime directory, which is not one of the four above
# and is the one place a stale socket can keep a client dialling nothing. The
# server derives it from os.TempDir(), which is $TMPDIR on macOS and /tmp on
# Linux — not a fixed /tmp.
RUNTIME_DIR=$(home_default "${XDG_RUNTIME_DIR:-}" "${TMPDIR:-/tmp}/discobox-$(id -u)")/discobox

say() { printf '  %s\n' "$*"; }

# What a Docker reset would take, counted — and counted again for the part that
# has nothing to do with Discobox, because "12 images" and "12 images, 9 of them
# nothing to do with this" are different sentences and only one of them is a
# warning.
docker_inventory() {
    if ! docker info >/dev/null 2>&1; then
        say "(no Docker daemon reachable)"
        return
    fi
    images=$(docker images -aq 2>/dev/null | wc -l | tr -d ' ')
    foreign=$(docker images --format '{{.Repository}}' 2>/dev/null | grep -cv discobox || true)
    containers=$(docker ps -aq 2>/dev/null | wc -l | tr -d ' ')
    volumes=$(docker volume ls -q 2>/dev/null | wc -l | tr -d ' ')
    say "$containers containers, $images images, $volumes volumes, and the build cache"
    if [ "${foreign:-0}" -gt 0 ]; then
        say "of which $foreign images are NOT Discobox's and will be deleted anyway"
    fi
}

# The distinct directories to remove, one per line. macOS puts data, config and
# state on one path, so this deduplicates: three identical lines read like three
# deletions.
#
# Read line by line by every caller, never through $(...): the macOS path is
# "~/Library/Application Support/discobox", and word splitting turns that one
# directory into two that are not it.
state_dirs() {
    printf '%s\n' "$DATA_DIR" "$CONFIG_DIR" "$CACHE_DIR" "$STATE_DIR" "$RUNTIME_DIR" | awk '!seen[$0]++'
}

# The server runs one of exactly two ways: the standalone binary, or the CLI's
# own subcommand. Matching anything looser than this is dangerous, not merely
# imprecise — an earlier version matched "discobox.* server", which any shell
# command mentioning both words satisfies, including the one running this
# script. pkill would then have killed the teardown mid-run.
#
# Self is excluded for the same reason: this script's own command line names the
# patterns it searches for.
server_pids() {
    pgrep -f 'discobox-server' 2>/dev/null || true
    pgrep -f 'discobox admin server' 2>/dev/null || true
}

running_servers() {
    self=$$
    parent=$PPID
    server_pids | sort -u | while read -r pid; do
        [ -z "$pid" ] && continue
        [ "$pid" = "$self" ] && continue
        [ "$pid" = "$parent" ] && continue
        printf '%s %s\n' "$pid" "$(ps -o args= -p "$pid" 2>/dev/null | head -c 100)"
    done
}

if [ "$DESTROY" -eq 0 ]; then
    echo "Would stop:"
    # Captured rather than piped: a pipeline reports the exit status of its last
    # command, so pgrep finding nothing would still look like success.
    running=$(running_servers)
    if [ -n "$running" ]; then
        printf '%s\n' "$running" | sed 's/^/  /'
    else
        say "(no server running)"
    fi
    echo "Would RESET DOCKER COMPLETELY — every container, image, volume and"
    echo "build cache on this daemon, not only Discobox's:"
    docker_inventory
    echo
    echo "  Anything else using this daemon loses it too. There is no filter."
    echo "Would delete:"
    state_dirs | while IFS= read -r dir; do
        if [ -e "$dir" ]; then
            say "$dir"
        else
            say "$dir (absent)"
        fi
    done
    echo
    echo "Re-run with --yes to do it."
    exit 0
fi

echo "==> stopping the server"
# Ask first: a server that shuts down cleanly releases its socket and stops its
# VMs, which a signal does not give it the chance to do.
if command -v discobox >/dev/null 2>&1; then
    discobox admin server shutdown --wait >/dev/null 2>&1 || true
fi
for pid in $(running_servers | cut -d" " -f1); do
    kill "$pid" 2>/dev/null || true
done
sleep 1
for pid in $(running_servers | cut -d" " -f1); do
    kill -9 "$pid" 2>/dev/null || true
done

echo "==> RESETTING DOCKER COMPLETELY (every container, image and volume,"
echo "    not only Discobox's)"
if docker info >/dev/null 2>&1; then
    docker ps -aq | xargs -r docker rm -f >/dev/null 2>&1 || true
    # Volumes before images: a volume in use by a container that is already gone
    # still pins nothing, but this order needs no retry either way.
    docker volume ls -q | xargs -r docker volume rm -f >/dev/null 2>&1 || true
    docker images -aq | xargs -r docker rmi -f >/dev/null 2>&1 || true
    # Build cache and dangling everything else, which the loops above do not
    # reach and which is most of what a BuildKit-heavy machine accumulates.
    docker system prune -af --volumes >/dev/null 2>&1 || true
    say "$(docker ps -aq | wc -l) containers, $(docker images -aq | wc -l) images, $(docker volume ls -q | wc -l) volumes left"
else
    say "no Docker daemon; skipped"
fi

echo "==> deleting Discobox state"
state_dirs | while IFS= read -r dir; do
    if [ -e "$dir" ]; then
        rm -rf "$dir"
        say "removed $dir"
    fi
done
# The development image watcher's manifest, which lives with the checkout rather
# than with the state directories.
if [ -e .tmp/discobox-dev-images.json ]; then
    rm -f .tmp/discobox-dev-images.json
    say "removed .tmp/discobox-dev-images.json"
fi

echo
echo "Done. The next server start is a first start."
