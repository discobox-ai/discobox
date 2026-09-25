#!/bin/sh
set -eu

# The harness-run convention (ADR 0086 §3): the runtime types
# `discobox-harness-run [--resume] '<prompt>'` into the terminal's login shell.
#
# The prompt trails a resume too (ADR 0086 §4), so a user whose first launch
# failed can still see what the sandbox was asked to do. This session already
# contains it, so `--continue` replaces it rather than sending it a second time.
if [ "${1-}" = "--resume" ]; then
	set -- --continue
else
	# A prompt is one prompt, however many words the shell split it into: the
	# command is typed, so `discobox new fix the failing tests` arrives here
	# as four arguments. Joining everything after the flags back together with
	# single spaces is the wrapper's half of the convention (ADR 0086 §3) —
	# `claude` takes its prompt as a single positional and would otherwise be
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

# Claude Code's memory lives in the primary source's durable data, keyed by the
# uid the way the pool cache is (ADR 0094): every sandbox on a source mounts the
# same tree whatever uid it runs as, so sandboxes that agree on a uid share
# memories, and one that does not can neither write the other's files nor take
# them over. The mount's root is chowned to whoever created a sandbox last, so
# the partition is made through sudo rather than depending on who owns its
# parent.
#
# The path names the uid, so the image's static managed settings cannot carry
# it. The launcher writes it as a managed-settings drop-in instead: policy, like
# the rest of the image's Claude Code settings, so a `claude` typed into any
# shell afterwards finds the same memory, `.claude/settings.json` (which the
# configure flow replaces) never holds it, and nothing that file says outranks
# it.
SOURCE_DATA=/.discobox/data-per-source/primary
MANAGED_DROP_INS=/etc/claude-code/managed-settings.d
USER_DATA="$SOURCE_DATA/users/$(id -u)"
MEMORIES="$USER_DATA/harnesses/claude-code/memories"
# Where memories lived before they were keyed by uid. They are carried over
# only to the uid that owns them, never claimed by another.
LEGACY_MEMORIES="$SOURCE_DATA/harnesses/claude-code/memories"

if [ -d "$SOURCE_DATA" ]; then
	if sudo -n install -d -m 0755 "$SOURCE_DATA/users" &&
		sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0700 "$USER_DATA"; then
		if [ ! -e "$MEMORIES" ] && [ -d "$LEGACY_MEMORIES" ] &&
			[ "$(stat -c %u "$LEGACY_MEMORIES")" = "$(id -u)" ]; then
			mkdir -p "$(dirname "$MEMORIES")"
			cp -a "$LEGACY_MEMORIES" "$MEMORIES" ||
				printf '%s\n' "discobox: could not carry over Claude Code memories from $LEGACY_MEMORIES" >&2
		fi
		mkdir -p "$MEMORIES"
		if ! sudo -n install -d -m 0755 "$MANAGED_DROP_INS" ||
			! printf '{"autoMemoryDirectory":"%s"}\n' "$MEMORIES" |
			sudo -n tee "$MANAGED_DROP_INS/discobox-memory.json" >/dev/null; then
			printf '%s\n' "discobox: could not record source-scoped Claude Code memories; using local storage" >&2
		fi
	else
		printf '%s\n' "discobox: could not prepare source-scoped Claude Code memories; using local storage" >&2
	fi
fi

exec claude "$@"
