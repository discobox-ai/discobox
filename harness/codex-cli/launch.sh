#!/bin/sh
set -eu

# Codex has no supported setting for moving only its consolidated memory
# workspace, and deliberately rejects a symlinked memory root. Keep CODEX_HOME
# (auth, config, sessions, and SQLite coordination) sandbox-local and bind the
# source-scoped backing directory onto a real memories directory.
SOURCE_DATA=/.discobox/data-per-source/primary
SHARED_MEMORIES="$SOURCE_DATA/harnesses/codex/memories"
CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
LOCAL_MEMORIES="$CODEX_HOME_DIR/memories"

if [ -d "$SOURCE_DATA" ]; then
	mkdir -p "$SHARED_MEMORIES" "$CODEX_HOME_DIR"
	# Replace links made by the older launcher, but never follow or replace a
	# link owned by something else.
	if [ -L "$LOCAL_MEMORIES" ]; then
		if [ "$(readlink "$LOCAL_MEMORIES")" = "$SHARED_MEMORIES" ]; then
			unlink "$LOCAL_MEMORIES"
		else
			printf '%s\n' "discobox: Codex memories already link elsewhere; leaving them unchanged" >&2
			exec codex "$@"
		fi
	fi
	mkdir -p "$LOCAL_MEMORIES"
	if ! mountpoint -q "$LOCAL_MEMORIES"; then
		if [ -n "$(find "$LOCAL_MEMORIES" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
			printf '%s\n' "discobox: Codex memories already exist locally; leaving them unchanged" >&2
		elif ! sudo -n mount --bind "$SHARED_MEMORIES" "$LOCAL_MEMORIES"; then
			printf '%s\n' "discobox: could not mount source-scoped Codex memories; using local storage" >&2
		fi
	fi
fi

exec codex "$@"
