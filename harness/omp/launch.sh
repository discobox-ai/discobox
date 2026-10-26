#!/bin/sh
set -eu

# The harness-run convention (ADR 0086 §3): the runtime types
# `discobox-harness-run [--resume] '<prompt>'` into the terminal's login shell.
#
# The prompt trails a resume too (ADR 0086 §4), so a user whose first launch
# failed can still see what the sandbox was asked to do. The resumed session
# already contains it, so `--continue` replaces it rather than re-sending it.
# With no session to continue, omp opens a new one.
if [ "${1-}" = "--resume" ]; then
	set -- --continue
elif [ "$#" -gt 0 ]; then
	# A prompt is one prompt, however many words the shell split it into: the
	# command is typed, so `discobox run . fix the failing tests` arrives here
	# as four arguments. Joining everything after the flags back together with
	# single spaces is the wrapper's half of the convention (ADR 0086 §3).
	#
	# omp sends each positional argument as a message of its own, and reads one
	# starting with @ as a file to attach. After `--` every argument is literal
	# text, so a prompt that leads with a dash or an @ stays a prompt.
	prompt="$1"
	shift
	for word in "$@"; do
		prompt="$prompt $word"
	done
	set -- -- "$prompt"
fi

# The egress proxy for omp's Anthropic transport (proxy-env.sh). The terminal's
# login shell has sourced it already; a launch from anywhere else has not.
if [ -r /etc/profile.d/omp-proxy.sh ]; then
	# shellcheck disable=SC1091 # Installed by the image (proxy-env.sh).
	. /etc/profile.d/omp-proxy.sh
fi

# Serialize imports with capture/repair. A local login is never replaced by
# an older delivered document; the acknowledged handoff owns that transition.
CREDENTIALS="$HOME/.config/discobox/omp-credentials.json"
if [ -s "$CREDENTIALS" ]; then
	mkdir -p "$HOME/.config/discobox"
	(
		flock -x 9
		if /usr/local/libexec/discobox/omp-capture guard; then
			if ! omp auth import "$CREDENTIALS" >/dev/null 2>&1; then
				printf '%s\n' "discobox: omp could not import the harness's credentials." >&2
			fi
		fi
	) 9>"$HOME/.config/discobox/harness-capture.lock"
fi

# --auto-approve approves every tool. The sandbox is the isolation boundary, so
# omp's own prompts would only guard a machine that exists to be written to —
# the same baseline the other images set. A flag rather than the settings
# overlay, so the user's per-tool `tools.approval` rules still apply
# (ADR 26-10-14-718 §4).
#
# --allow-home keeps omp where Discobox started it: started in $HOME, omp
# otherwise moves itself to a temporary directory.
exec omp --auto-approve --allow-home "$@"
