#!/bin/sh
set -eu

# The harness-run convention (ADR 0086 §3): the runtime types
# `discobox-harness-run [--resume] '<prompt>'` into the terminal's login shell.
#
# The prompt trails a resume too (ADR 0086 §4), so a user whose first launch
# failed can still see what the sandbox was asked to do. The resumed session
# already contains it, so `--continue` replaces it rather than re-sending it.
# With no session to continue, pi opens a new one.
if [ "${1-}" = "--resume" ]; then
	set -- --continue
elif [ "$#" -gt 0 ]; then
	# A prompt is one prompt, however many words the shell split it into: the
	# command is typed, so `discobox run . fix the failing tests` arrives here
	# as four arguments. Joining everything after the flags back together with
	# single spaces is the wrapper's half of the convention (ADR 0086 §3).
	#
	# pi sends each positional argument as a message of its own, so the prompt
	# is one argument, after `--` so a dash-led prompt stays a prompt. `--` does
	# not stop pi reading an argument that starts with @ as a file to attach,
	# though, so such a prompt leads with a space, which pi sends as it is
	# (ADR 26-10-15-259 §4).
	prompt="$1"
	shift
	for word in "$@"; do
		prompt="$prompt $word"
	done
	case "$prompt" in
	@*) prompt=" $prompt" ;;
	esac
	set -- -- "$prompt"
fi

# --approve trusts the project's own pi settings and resources for this
# process. Without it pi asks, in a dialog that stalls the launch, whenever the
# directory holds .pi/ settings or .agents/skills — which a repository keeping
# its skills there always does. The sandbox is the isolation boundary, the same
# baseline the other images set; pi asks no approval before a tool call at all.
exec pi --approve "$@"
