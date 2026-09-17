#!/bin/sh
set -eu

# discobox-prompt: the one-shot prompting interface an in-sandbox tool uses to
# ask this harness's model a question (ADR 0079). Its first consumer is the
# credential CLI's judge, which will not run a wrapped command until a model
# agrees the command is the use a human approved.
#
# Contract:
#   discobox-prompt --model ROLE --system TEXT --prompt TEXT --output-schema JSON [--no-tools]
#   stdout: the model's answer, and, when a schema is given, one JSON document
#           conforming to it. `opencode run` frames its answer in a transcript,
#           so a caller parsing a schema'd answer must find the JSON in it.
#   exit 0: the model answered. Anything else: it did not.
#
# --model names a role, never a model id: the caller does not know what this
# image installed. Mapping the role is this script's job.
#
# --no-tools means the model answers from its prompt and executes nothing: no
# command, no file read, no network fetch (ADR 0090). It is this script's job
# to map that onto whatever its CLI calls the same thing, the way --model
# already is.

model=""
system=""
prompt=""
schema=""
no_tools=""

while [ $# -gt 0 ]; do
	case "$1" in
	--model) model="${2:-}"; shift 2 ;;
	--model=*) model="${1#--model=}"; shift ;;
	--system) system="${2:-}"; shift 2 ;;
	--system=*) system="${1#--system=}"; shift ;;
	--prompt) prompt="${2:-}"; shift 2 ;;
	--prompt=*) prompt="${1#--prompt=}"; shift ;;
	--output-schema) schema="${2:-}"; shift 2 ;;
	--output-schema=*) schema="${1#--output-schema=}"; shift ;;
	--no-tools) no_tools=1; shift ;;
	--) shift; break ;;
	-*) printf '%s\n' "discobox-prompt: unknown flag $1" >&2; exit 2 ;;
	*) break ;;
	esac
done

if [ -z "$prompt" ] && [ $# -gt 0 ]; then
	prompt="$*"
fi
if [ -z "$prompt" ]; then
	printf '%s\n' "discobox-prompt: no prompt given" >&2
	exit 2
fi

# opencode run takes one message and no separate system channel, so the system
# text leads the prompt and the schema closes it.
composed="$prompt"
if [ -n "$system" ]; then
	composed="$(printf '%s\n\n%s\n' "$system" "$prompt")"
fi
if [ -n "$schema" ]; then
	composed="$(printf '%s\n\n%s\n%s\n' "$composed" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi

# The judge is the model named by `judgeModel` in Discobox's settings for this
# harness (.config/discobox/opencode-harness.json), else the last model picked
# with /models, else opencode's own pick among the connected providers. The user
# chooses the providers here, so no fixed model is one this harness can be sure
# to reach, and a judge that cannot answer refuses every command it is asked
# about.
#
# Both named sources are files the judged agent can write, which is the gap a
# pinned model would close: the agent could point the judge at another model,
# and with a provider key of its own in auth.json, at a provider it chose. The
# agent has sudo, so no file in the sandbox holds against a deliberate one
# (ADR 0090); what the tool-free run below does guarantee is that the judge
# does not run inside configuration, plugins or instructions the agent wrote.
# The choice is recorded in ADR 0127 §4.
SETTINGS="$HOME/.config/discobox/opencode-harness.json"
MODEL_PREFERENCE="${XDG_STATE_HOME:-$HOME/.local/state}/opencode/model.json"
judge_model() {
	named=""
	if [ -f "$SETTINGS" ]; then
		named=$(jq -r '.judgeModel // "" | strings' "$SETTINGS" 2>/dev/null) || named=""
	fi
	if [ -z "$named" ] && [ -f "$MODEL_PREFERENCE" ]; then
		named=$(jq -r '.recent[0] // empty | "\(.providerID)/\(.modelID)"' "$MODEL_PREFERENCE" 2>/dev/null) || named=""
	fi
	printf '%s' "$named"
}

set -- opencode run
case "$model" in
judge)
	named=$(judge_model)
	if [ -n "$named" ]; then
		set -- "$@" --model "$named"
	fi
	;;
fast | "") ;;
*) set -- "$@" --model "$model" ;;
esac

if [ -n "$no_tools" ]; then
	# The model answers from its prompt and executes nothing, and nothing the
	# caller can write reaches it. The caller is the agent this answer may be
	# judging, running as the same user in the same directory, so every source
	# opencode would otherwise read is one it could have written: a config
	# file's permission rules (merged before ours, and the last matching rule
	# wins), a plugin (code in this process, whatever the permissions say), an
	# AGENTS.md or CLAUDE.md found from the working directory, a cached model
	# catalog pointing a provider somewhere else, and any OPENCODE_* variable.
	# claude-code's wrapper reaches the same place with --restricted.
	#
	# So opencode runs from an empty directory, with empty config, state and
	# cache directories, project configuration and Claude Code files off,
	# plugins off (--pure), and no inherited OPENCODE_* variable.
	#
	# Its data directory is its own too, holding only the credentials: the api
	# and oauth entries of auth.json. A `wellknown` entry is not a credential
	# but a URL whose /.well-known/opencode opencode fetches and merges in as
	# configuration — permission rules, providers, endpoints — past both
	# switches above, so it is the one part of that file left out.
	isolated=$(mktemp -d)
	trap 'rm -rf "$isolated"' EXIT
	mkdir -p "$isolated/cwd" "$isolated/config" "$isolated/state" "$isolated/cache" "$isolated/data/opencode"
	auth="${XDG_DATA_HOME:-$HOME/.local/share}/opencode/auth.json"
	if [ -f "$auth" ]; then
		jq 'with_entries(select(.value.type == "api" or .value.type == "oauth"))' "$auth" \
			>"$isolated/data/opencode/auth.json" 2>/dev/null || rm -f "$isolated/data/opencode/auth.json"
	fi
	for name in $(env | sed -n 's/^\(OPENCODE_[A-Za-z0-9_]*\)=.*/\1/p'); do
		unset "$name"
	done
	export XDG_CONFIG_HOME="$isolated/config" XDG_STATE_HOME="$isolated/state" XDG_CACHE_HOME="$isolated/cache" XDG_DATA_HOME="$isolated/data"
	export OPENCODE_DISABLE_PROJECT_CONFIG=1 OPENCODE_DISABLE_CLAUDE_CODE=1 OPENCODE_DISABLE_AUTOUPDATE=1
	# With no configuration left to merge into, one rule denying every
	# permission is the whole rule set.
	export OPENCODE_PERMISSION='{"*":"deny"}'
	cd "$isolated/cwd"
	set -- "$@" --pure
	# Not exec: the isolated directory is removed when opencode exits.
	"$@" "$composed"
	exit
else
	# Nobody is there to answer a permission prompt.
	set -- "$@" --auto
fi

exec "$@" "$composed"
