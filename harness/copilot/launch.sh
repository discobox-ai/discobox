#!/bin/sh
set -eu

# The harness-run convention (ADR 0086 §3): the runtime types
# `discobox-harness-run [--resume] '<prompt>'` into the terminal's login shell.
#
# The prompt trails a resume too (ADR 0086 §4), so a user whose first launch
# failed can still see what the sandbox was asked to do. The resumed session
# already contains it, so `--continue` replaces it rather than re-sending it.
# With no session to continue, Copilot simply opens a new one.
if [ "${1-}" = "--resume" ]; then
	set -- --continue
elif [ "$#" -gt 0 ]; then
	# A prompt is one prompt, however many words the shell split it into: the
	# command is typed, so `discobox new fix the failing tests` arrives here
	# as four arguments. Joining everything after the flags back together with
	# single spaces is the wrapper's half of the convention (ADR 0086 §3).
	#
	# Copilot takes no positional prompt. --interactive starts the TUI and
	# submits the prompt; the `=` form keeps a prompt that starts with a dash
	# from being read as another flag.
	prompt="$1"
	shift
	for word in "$@"; do
		prompt="$prompt $word"
	done
	set -- "--interactive=$prompt"
fi

# --allow-all approves every tool, path and URL. The sandbox is the isolation
# boundary, so Copilot's own prompts would only guard a machine that exists to
# be written to — the same baseline the other images set. It is a flag because
# the image's COPILOT_ALLOW_ALL=true, which trusts the directory Copilot starts
# in, leaves an interactive session on manual approval (ADR 26-10-02-840 §4).
exec copilot --allow-all "$@"
