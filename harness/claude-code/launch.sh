#!/bin/sh
set -eu

# The harness-run convention (ADR 0086 §3): the runtime types
# `discobox-harness-run [--resume] '<prompt>'` into the terminal's login shell.
#
# The prompt trails a resume too (ADR 0086 §4), so a user whose first launch
# failed can still see what the sandbox was asked to do. This session already
# contains it, so `--continue` replaces it rather than sending it a second time.
#
# A primary relaunched before anyone sent its harness a message (a re-attach
# after a stop, the tour's hand-off) has no conversation, and `claude
# --continue` then prints "No conversation found to continue" and exits 1,
# leaving the user at the login shell (issue #1421). So a resume continues only
# when Claude Code would find a conversation for this directory, and otherwise
# starts a fresh session with the prompt. The first launch hands Claude Code the
# prompt as its first message, which is what records a conversation, so none
# means the prompt never reached it (the launch failed, or stopped at sign-in):
# the case ADR 0086 §4 carries the prompt for, and it cannot be sent twice.
#
# Claude Code keeps a directory's conversations as projects/<name>/*.jsonl,
# where the name is the cwd with every character outside [A-Za-z0-9] made "-",
# and a name over 200 characters is its first 200, "-" and a hash, found by
# that prefix (Claude Code 2.1.331). A conversation file is written with its
# first message. The name is computed in JavaScript, as Claude Code does, so a
# non-ASCII path counts characters, not bytes.
has_conversation() {
	node -e '
const fs = require("fs");
const path = require("path");
const projects = process.argv[1];
const name = process.cwd().replace(/[^a-zA-Z0-9]/g, "-");
let dirs = [name];
if (name.length > 200) {
	const prefix = name.slice(0, 200) + "-";
	try {
		dirs = fs.readdirSync(projects).filter((dir) => dir.startsWith(prefix));
	} catch {
		dirs = [];
	}
}
const found = dirs.some((dir) => {
	try {
		return fs.readdirSync(path.join(projects, dir)).some((file) => file.endsWith(".jsonl"));
	} catch {
		return false;
	}
});
process.exit(found ? 0 : 1);
' "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects"
}

resume=
if [ "${1-}" = "--resume" ]; then
	shift
	if has_conversation; then
		resume=1
		set -- --continue
	fi
fi
if [ -z "$resume" ]; then
	# A prompt is one prompt, however many words the shell split it into: the
	# command is typed, so `discobox run . fix the failing tests` arrives here
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
# shell afterwards finds the same memory, `.claude/settings.json` (which a
# saved harness change replaces) never holds it, and nothing that file says outranks
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

/usr/local/libexec/discobox/claude-code-capture init
exec claude "$@"
