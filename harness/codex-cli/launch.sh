#!/bin/sh
set -eu

# Codex has no supported setting for moving only its consolidated memory
# workspace. Keep CODEX_HOME (auth, config, sessions, and SQLite coordination)
# sandbox-local and link only the memories directory into the primary source's
# opaque durable data mount.
SOURCE_DATA=/.discobox/data-per-source/primary
SHARED_MEMORIES="$SOURCE_DATA/harnesses/codex/memories"
CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
LOCAL_MEMORIES="$CODEX_HOME_DIR/memories"

if [ -d "$SOURCE_DATA" ]; then
	mkdir -p "$SHARED_MEMORIES" "$CODEX_HOME_DIR"
	if [ ! -e "$LOCAL_MEMORIES" ] && [ ! -L "$LOCAL_MEMORIES" ]; then
		if ! ln -s "$SHARED_MEMORIES" "$LOCAL_MEMORIES" 2>/dev/null; then
			# Two terminals can start together. The winner's correct link makes
			# the loser successful too; anything else is left untouched below.
			if [ ! -L "$LOCAL_MEMORIES" ] || [ "$(readlink "$LOCAL_MEMORIES")" != "$SHARED_MEMORIES" ]; then
				printf '%s\n' "discobox: Codex memories appeared at another location; leaving them unchanged" >&2
			fi
		fi
	elif [ -L "$LOCAL_MEMORIES" ] && [ "$(readlink "$LOCAL_MEMORIES")" != "$SHARED_MEMORIES" ]; then
		printf '%s\n' "discobox: Codex memories already link elsewhere; leaving them unchanged" >&2
	elif [ ! -L "$LOCAL_MEMORIES" ]; then
		printf '%s\n' "discobox: Codex memories already exist locally; leaving them unchanged" >&2
	fi
fi

exec codex "$@"
