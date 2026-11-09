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
	# command is typed, so `discobox run . fix the failing tests` arrives here
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

# Trust the actual cwd in the sandbox's system layer, including restarts and
# source-less boxes. A later direct codex start sees the same trust. Serialize
# starts from different folders so neither loses the other's system entry.
sudo -n flock /run/lock/discobox-codex-trust.lock \
	node /usr/local/libexec/discobox/codex-trust.cjs "$PWD"

# Codex copies to the native clipboard first and writes OSC 52 to its terminal
# only when it sees an SSH (or tmux) session. The image's DISPLAY=:0 gives it a
# native clipboard that works — the box's own X server, which nothing outside
# the box reads — so without this a copy succeeds into that and never reaches
# the user's terminal. A sandbox is a remote session in every sense Codex
# means, and Codex tests only that the variable is set; the value is the
# variable's documented shape ("client_ip client_port server_ip server_port")
# with nothing real to put in it. A real SSH session's value is kept.
export SSH_CONNECTION="${SSH_CONNECTION:-127.0.0.1 0 127.0.0.1 0}"

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
SOURCE_DATA="/.discobox/data-per-source/primary"
USER_DATA="$SOURCE_DATA/users/$(id -u)"
SHARED_MEMORIES="$USER_DATA/harnesses/codex/memories"
# Where memories lived before they were keyed by uid. They are carried over
# only to the uid that owns them, never claimed by another.
LEGACY_MEMORIES="$SOURCE_DATA/harnesses/codex/memories"
CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
LOCAL_MEMORIES="$CODEX_HOME_DIR/memories"

# Any step that cannot be taken leaves memories local, and Codex still starts.
share_memories() {
	[ -d "$SOURCE_DATA" ] || return 0
	if ! sudo -n install -d -m 0755 "$SOURCE_DATA/users" ||
		! sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0700 "$USER_DATA"; then
		printf '%s\n' "discobox: could not prepare source-scoped Codex memories; using local storage" >&2
		return 0
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
			return 0
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
}
share_memories

# Codex's TUI exits at bootstrap when its first requests cannot connect: a
# signed-in start asks the ChatGPT backend for the account's workspace, and a
# failure there ends Codex with "account/read failed ... workspace routing
# discovery failed" and status 1 (issue #767). A box's first request out can
# meet a transient failure, and a launch that ends there ends the box's job, so
# a start that failed before it began a session is made again.
#
# Only status 1 is retried: 0 is a clean exit (also how a user quits the
# sign-in screen), over 128 is a signal (the user or the terminal ended it),
# and anything else (a usage error, a missing binary) would fail the same way
# again. A session began if a rollout appeared under sessions/ during the
# attempt, or one grew (a resume appends to the rollout it resumes): the prompt
# is in it, so starting again would send it twice, and the failure is the
# session's, not the start's. The rollouts are compared by name and size, not
# by time, so no clock has to agree with another.
SESSIONS="$CODEX_HOME_DIR/sessions"
DELAYS="2 5 10 20"
ATTEMPTS=1
for delay in $DELAYS; do ATTEMPTS=$((ATTEMPTS + 1)); done
attempt=1
for delay in $DELAYS ''; do
	before=$(find "$SESSIONS" -type f -exec wc -c {} + 2>/dev/null | sort)
	status=0
	codex "$@" || status=$?
	if [ "$status" -ne 1 ] || [ -z "$delay" ] ||
		[ "$(find "$SESSIONS" -type f -exec wc -c {} + 2>/dev/null | sort)" != "$before" ]; then
		exit "$status"
	fi
	attempt=$((attempt + 1))
	printf '%s\n' "discobox: Codex exited before its session started (status 1); starting it again in ${delay}s (attempt $attempt of $ATTEMPTS)" >&2
	sleep "$delay"
done
