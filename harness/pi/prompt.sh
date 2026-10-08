#!/bin/sh
set -eu

# discobox-prompt: the one-shot prompting interface an in-sandbox tool uses to
# ask this harness's model a question (ADR 0079). Its first consumer is the
# credential CLI's judge, which will not run a wrapped command until a model
# agrees the command is the use a human approved.
#
# Contract:
#   discobox-prompt --model ROLE --system TEXT --prompt TEXT --output-schema JSON [--no-tools]
#   stdout: the model's answer, and, when a schema is given, nothing but one
#           JSON document conforming to it. `pi --print` says only what the
#           model said, but a model asked for JSON may still fence it, so a
#           schema'd answer goes through discobox-prompt-answer: the promise is
#           the wrapper's to keep, not the model's to remember.
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

# pi has no schema flag, so the schema is stated as an instruction. It is
# appended last, after any caller system text, so it is the final word on the
# output shape. --system-prompt replaces pi's own coding-assistant prompt,
# which a judge has no use for.
if [ -n "$schema" ]; then
	system="$(printf '%s\n\n%s\n%s\n' "$system" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi

# The judge is the model named by `judgeModel` in Discobox's settings for this
# harness (.config/discobox/pi-harness.json), else the model pi itself starts
# with: `defaultProvider`/`defaultModel` in its global settings, else pi's own
# pick among the providers signed in. The user chooses the providers here, so
# no fixed model is one this harness can be sure to reach, and a judge that
# cannot answer refuses every command it is asked about. The judge then runs
# isolated from that configuration (below), so the model is read from it here,
# beforehand, and nothing else of it is.
#
# Both named sources are files the judged agent can write, which is the gap a
# pinned model would close. The agent has sudo, so no file in the sandbox
# holds against a deliberate one (ADR 0090); what the tool-free run below does
# guarantee is that the judge does not run inside configuration, extensions or
# instructions the agent wrote. The trade is the one ADR 0127 §4 records for
# opencode.
SETTINGS="$HOME/.config/discobox/pi-harness.json"
AGENT_DIR="${PI_CODING_AGENT_DIR:-$HOME/.pi/agent}"

judge_model() {
	named=""
	if [ -f "$SETTINGS" ]; then
		named=$(jq -r '.judgeModel // "" | strings' "$SETTINGS" 2>/dev/null) || named=""
	fi
	if [ -z "$named" ] && [ -f "$AGENT_DIR/settings.json" ]; then
		# provider/model when both are set, the model alone when only it is:
		# the same spelling pi's --model reads.
		named=$(jq -r '
			(.defaultModel // "" | strings) as $model
			| (.defaultProvider // "" | strings) as $provider
			| if $model == "" then "" elif $provider == "" then $model else "\($provider)/\($model)" end
		' "$AGENT_DIR/settings.json" 2>/dev/null) || named=""
	fi
	printf '%s' "$named"
}

# The judge answers without extended thinking, which its verdict of a few
# dozen tokens has no use for and which pi turns on by default. Set here rather
# than inherited, so neither the image's settings nor a caller decides how the
# gate thinks.
set -- pi --print --no-session --thinking off
case "$model" in
judge)
	named=$(judge_model)
	# pi takes an option's value as the next word and has no --model=NAME
	# spelling, so a value that begins with a dash would be read as an option
	# of its own. A model name never begins with one; a value from a file the
	# agent can write that does is dropped, and the judge runs on pi's own
	# pick, rather than handed to pi as a flag.
	case "$named" in
	-*) named="" ;;
	esac
	if [ -n "$named" ]; then
		set -- "$@" --model "$named"
	fi
	;;
fast | "") ;;
-*) printf '%s\n' "discobox-prompt: model $model is not a model name" >&2; exit 2 ;;
*) set -- "$@" --model "$model" ;;
esac
if [ -n "$system" ]; then
	set -- "$@" --system-prompt "$system"
fi

if [ -n "$no_tools" ]; then
	# The model answers from its prompt and executes nothing, and nothing the
	# caller can write reaches it. The caller is the agent this answer may be
	# judging, running as the same user in the same directory, so every source
	# pi would otherwise read is one it could have written: an extension (code
	# in this process), a skill or prompt template, an AGENTS.md or CLAUDE.md
	# found from the working directory, a project's .pi directory, and any
	# PI_* variable. claude-code's wrapper reaches the same place with
	# --restricted.
	#
	# So pi runs from an empty directory, with an agent directory of its own
	# holding only the credentials, every discovery off, project files
	# ignored, no inherited PI_* variable, and no network beside the model's:
	# PI_OFFLINE keeps the update check and the install ping out of a run
	# that exists for one answer.
	#
	# The credentials are auth.json's api_key and oauth entries. An api_key
	# whose key begins with ! is a command pi runs to obtain the key, which is
	# code the agent could have written, so it is the one entry left out.
	isolated=$(mktemp -d)
	trap 'rm -rf "$isolated"' EXIT
	mkdir -p "$isolated/cwd" "$isolated/agent"
	if [ -f "$AGENT_DIR/auth.json" ]; then
		jq 'with_entries(select(
			(.value.type == "oauth")
			or (.value.type == "api_key" and (.value.key | type) == "string" and (.value.key | startswith("!") | not))
		))' "$AGENT_DIR/auth.json" >"$isolated/agent/auth.json" 2>/dev/null || rm -f "$isolated/agent/auth.json"
	fi
	for name in $(env | sed -n 's/^\(PI_[A-Za-z0-9_]*\)=.*/\1/p'); do
		unset "$name"
	done
	export PI_CODING_AGENT_DIR="$isolated/agent" PI_OFFLINE=1
	cd "$isolated/cwd"
	set -- "$@" --no-tools --no-extensions --no-skills --no-prompt-templates --no-themes --no-context-files --no-approve
fi

# `--` so a prompt that begins with a dash or an @ is the prompt and not a
# flag or a file. Stdin is closed: in print mode pi reads whatever is piped
# to it into the prompt, and the judge's prompt is the one it was given.
set -- "$@" -- "$prompt"

if [ -n "$schema" ]; then
	# Captured rather than piped: a pipeline reports the exit status of its
	# last command, so pi failing would arrive as this script succeeding at
	# printing nothing.
	answer=$("$@" </dev/null) || exit
	printf '%s\n' "$answer" | discobox-prompt-answer
	exit
fi

# Not exec: an isolated directory is removed when pi exits.
"$@" </dev/null
