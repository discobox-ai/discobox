#!/bin/sh
set -eu

# The harness-run convention (ADR 0086 §3): the runtime types
# `discobox-harness-run [--resume] '<prompt>'` into the terminal's login shell.
#
# The prompt trails a resume too (ADR 0086 §4), so a user whose first launch
# failed can still see what the sandbox was asked to do. The resumed session
# already contains it, so `resume --last` replaces it rather than re-sending it.
if [ "${1-}" = "--resume" ]; then
	set -- resume --last
else
	# A prompt is one prompt, however many words the shell split it into: the
	# command is typed, so `discobox fix the failing tests` arrives here as
	# four arguments. Joining everything after the flags back together with
	# single spaces is the wrapper's half of the convention (ADR 0086 §3) —
	# `codex` takes its prompt as a single positional and would otherwise be
	# asked to "fix".
	if [ "$#" -gt 1 ]; then
		prompt="$1"
		shift
		for word in "$@"; do
			prompt="$prompt $word"
		done
		set -- "$prompt"
	fi
fi

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
