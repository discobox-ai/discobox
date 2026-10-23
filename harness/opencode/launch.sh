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
	# command is typed, so `discobox run . fix the failing tests` arrives here
	# as four arguments. Joining everything after the flags back together with
	# single spaces is the wrapper's half of the convention (ADR 0086 §3).
	#
	# opencode's positional argument is the project directory, not a prompt,
	# so the prompt goes to --prompt, which the TUI submits once it is ready.
	prompt="$1"
	shift
	for word in "$@"; do
		prompt="$prompt $word"
	done
	set -- --prompt "$prompt"
fi

# The configured credentials live in opencode's database, which only opencode
# writes (ADR 26-10-15-848 §3). A failure is said and not fatal: opencode still
# runs, on whatever it already holds or on its free models.
if ! /usr/local/libexec/discobox/opencode-credentials sync; then
	printf '%s\n' "discobox: could not load the configured opencode credentials; starting without them" >&2
fi

# Web search is Discobox's setting (.config/discobox/opencode-harness.json),
# answered during configure. opencode 2 searches the web with every provider
# by default, so only "off" needs saying, and a permission rule denying the
# tool is what removes it from what the model is offered.
SETTINGS="$HOME/.config/discobox/opencode-harness.json"
if [ -f "$SETTINGS" ] && [ "$(jq -r '.webSearch == false' "$SETTINGS" 2>/dev/null || true)" = true ]; then
	export OPENCODE_CONFIG_CONTENT='{"permission":{"websearch":"deny"}}'
fi

# --standalone runs opencode's server inside this process rather than in the
# per-user background service. The service outlives the terminal that started
# it, and the hook plugin publishes for the terminal whose environment it runs
# in, so under a shared service every terminal's hooks would be the first's
# (ADR 26-10-15-848 §2).
#
# --auto approves every tool use a rule does not deny. The sandbox is the
# isolation boundary, so opencode's own prompts would only guard a machine that
# exists to be written to — the same baseline the claude-code and codex images
# set. A flag rather than configuration: opencode's system layer
# (/etc/opencode) is its *managed* configuration, which outranks a project's own
# rules, and the user's global config is replaced by the configure flow's
# capture.
exec opencode --standalone --auto "$@"
