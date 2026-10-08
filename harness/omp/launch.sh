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
	# omp joins its positional messages itself, but reads any that begins with
	# @ as a file to attach, and any that names one of its subcommands as that
	# command, which a word of the prompt must never become; one argument
	# after `--` is the prompt and nothing else.
	prompt="$1"
	shift
	for word in "$@"; do
		prompt="$prompt $word"
	done
	set -- -- "$prompt"
fi

# The configured OAuth sign-ins are delivered as a file and imported into
# omp's store before every launch (see import.sh); API keys reach omp through
# .omp/agent/models.yml, which names the environment variable each secret is
# exported as, and need no step here.
AUTH_FILE="$HOME/.omp/agent/discobox-auth.json"
if [ -f "$AUTH_FILE" ]; then
	sh /usr/local/libexec/discobox/omp-import-credentials "$AUTH_FILE" || true
fi

# omp's lifecycle is published by an image-owned extension, loaded with --hook.
# omp's configuration overlay (/etc/omp/discobox.yml) could name it, but an
# overlay's `extensions` list replaces the user's rather than adding to it.
HOOK_EXTENSION=/usr/local/libexec/discobox/omp-hook-extension.js

# The policy baseline — approvals off, the update check off — is the overlay
# the image's env names in PI_CONFIG_FILES, so a plain `omp` typed into any
# shell gets it too.
exec omp --hook "$HOOK_EXTENSION" "$@"
