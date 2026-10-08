#!/bin/sh
set -eu

# The harness-run convention (ADR 0086 §3): the runtime types
# `discobox-harness-run [--resume] '<prompt>'` into the terminal's login shell.
#
# The prompt trails a resume too (ADR 0086 §4), so a user whose first launch
# failed can still see what the sandbox was asked to do. The resumed session
# already contains it, so `--continue` replaces it rather than re-sending it.
if [ "${1-}" = "--resume" ]; then
	set -- --continue
elif [ "$#" -gt 0 ]; then
	# A prompt is one prompt, however many words the shell split it into: the
	# command is typed, so `discobox new fix the failing tests` arrives here
	# as four arguments. Joining everything after the flags back together with
	# single spaces is the wrapper's half of the convention (ADR 0086 §3).
	#
	# pi joins its positional messages itself, but reads any that begins with
	# @ as a file to attach, which a word of the prompt must never become; one
	# argument after `--` is the prompt and nothing else, and `--` keeps a
	# prompt that begins with a dash from being read as a flag.
	prompt="$1"
	shift
	for word in "$@"; do
		prompt="$prompt $word"
	done
	set -- -- "$prompt"
fi

# Pi's lifecycle is published by an image-owned extension. Pi has no managed
# configuration layer to name it from — its settings are the user's global
# file, which the configure flow replaces with the user's captured copy, and a
# project's — so the launcher loads it with --extension, which pi honours even
# under --no-extensions.
HOOK_EXTENSION=/usr/local/libexec/discobox/pi-hook-extension.js

# --approve trusts the project's own .pi configuration for this run. The
# sandbox is the isolation boundary, so pi's trust dialog would only guard a
# machine that exists to be written to — the same baseline the claude-code and
# codex images set with their trust stanzas.
exec pi --approve --extension "$HOOK_EXTENSION" "$@"
