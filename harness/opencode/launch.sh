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
	# command is typed, so `discobox run fix the failing tests` arrives here
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

# --auto approves every tool use a rule does not deny. The sandbox is the
# isolation boundary, so opencode's own prompts would only guard a machine that
# exists to be written to — the same baseline the claude-code and codex images
# set. A flag rather than configuration: opencode's system layer
# (/etc/opencode) is its *managed* configuration, which outranks a project's own
# rules, and the user's global config is replaced by the configure flow's
# capture.
#
# Web search is Discobox's setting (.config/discobox/opencode-harness.json),
# answered during configure. opencode searches with keyless Exa and Parallel,
# but only for models from its own providers unless these variables turn it on
# for every provider; turning it off is a permission rule, since for those
# providers nothing else does.
SETTINGS="$HOME/.config/discobox/opencode-harness.json"
if [ -f "$SETTINGS" ]; then
	case "$(jq -r '.webSearch | if . == true then "on" elif . == false then "off" else "" end' "$SETTINGS" 2>/dev/null || true)" in
	on)
		export OPENCODE_ENABLE_EXA=1 OPENCODE_ENABLE_PARALLEL=1
		;;
	off)
		export OPENCODE_PERMISSION='{"websearch":"deny"}'
		;;
	esac
fi

exec opencode --auto "$@"
