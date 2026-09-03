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
	# command is typed, so `discobox fix the failing tests` arrives here as
	# four arguments. Joining everything after the flags back together with
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

# Claude Code supports an explicit auto-memory directory. Supply it at launch
# rather than putting it in the captured user settings file, so reconfigure can
# continue replacing that file without dropping Discobox's storage wiring.
SOURCE_DATA=/.discobox/data-per-source/primary
SHARED_MEMORIES="$SOURCE_DATA/harnesses/claude-code/memories"

if [ -d "$SOURCE_DATA" ]; then
	mkdir -p "$SHARED_MEMORIES"
	exec claude --settings "{\"autoMemoryDirectory\":\"$SHARED_MEMORIES\"}" "$@"
fi

exec claude "$@"
