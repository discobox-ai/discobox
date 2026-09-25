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
	# command is typed, so `discobox new fix the failing tests` arrives here
	# as four arguments. Joining everything after the flags back together with
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
#
# Source data is shared by every sandbox on the source, whatever uid each runs
# as, so memories are keyed by the uid the way the pool cache is (ADR 0094):
# sandboxes that agree on a uid share them, and one that does not can neither
# write the other's files nor take them over. The mount's root is chowned to
# whoever created a sandbox last, so the partition is made through sudo rather
# than depending on who owns its parent.
SOURCE_DATA=/.discobox/data-per-source/primary
USER_DATA="$SOURCE_DATA/users/$(id -u)"
SHARED_MEMORIES="$USER_DATA/harnesses/codex/memories"
# Where memories lived before they were keyed by uid. They are carried over
# only to the uid that owns them, never claimed by another.
LEGACY_MEMORIES="$SOURCE_DATA/harnesses/codex/memories"
CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
LOCAL_MEMORIES="$CODEX_HOME_DIR/memories"

if [ -d "$SOURCE_DATA" ]; then
	if ! sudo -n install -d -m 0755 "$SOURCE_DATA/users" ||
		! sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0700 "$USER_DATA"; then
		printf '%s\n' "discobox: could not prepare source-scoped Codex memories; using local storage" >&2
		exec codex "$@"
	fi
	if [ ! -e "$SHARED_MEMORIES" ] && [ -d "$LEGACY_MEMORIES" ] &&
		[ "$(stat -c %u "$LEGACY_MEMORIES")" = "$(id -u)" ]; then
		mkdir -p "$(dirname "$SHARED_MEMORIES")"
		cp -a "$LEGACY_MEMORIES" "$SHARED_MEMORIES" ||
			printf '%s\n' "discobox: could not carry over Codex memories from $LEGACY_MEMORIES" >&2
	fi
	mkdir -p "$SHARED_MEMORIES" "$CODEX_HOME_DIR"
	# Replace links made by the older launcher, but never follow or replace a
	# link owned by something else.
	if [ -L "$LOCAL_MEMORIES" ]; then
		case "$(readlink "$LOCAL_MEMORIES")" in
		"$SHARED_MEMORIES" | "$LEGACY_MEMORIES")
			unlink "$LOCAL_MEMORIES"
			;;
		*)
			printf '%s\n' "discobox: Codex memories already link elsewhere; leaving them unchanged" >&2
			exec codex "$@"
			;;
		esac
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
