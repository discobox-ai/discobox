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
	# command is typed, so `discobox new fix the failing tests` arrives here
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

# The credentials the harness config holds reach omp as an `omp auth export`
# document, templated with this sandbox's sentinels, and omp keeps credentials
# in SQLite, so they go in through its own `omp auth import` (ADR 26-10-14-718
# §2). Every launch imports it again: an import replaces the same provider's
# key and the same account's login, so this is idempotent, and a login the
# sandbox replaced in place is restored. A failed import is reported and omp
# starts anyway, signed in to whatever it already had.
CREDENTIALS="$HOME/.config/discobox/omp-credentials.json"
if [ -s "$CREDENTIALS" ]; then
	if ! imported=$(omp auth import "$CREDENTIALS" 2>&1); then
		printf '%s\n' "discobox: omp could not import the harness's credentials:" "$imported" >&2
	fi
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
