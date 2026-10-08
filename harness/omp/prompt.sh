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
#           JSON document conforming to it. `omp --print` says only what the
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

# omp has no schema flag, so the schema is stated as an instruction. It is
# appended last, after any caller system text, so it is the final word on the
# output shape. --system-prompt replaces omp's own coding-assistant prompt,
# which a judge has no use for.
if [ -n "$schema" ]; then
	system="$(printf '%s\n\n%s\n%s\n' "$system" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi

# The judge is the model named by `judgeModel` in Discobox's settings for this
# harness (.config/discobox/omp-harness.json), else the model omp itself starts
# with: its `default` model role, which /model and the setup wizard record in
# its configuration, else omp's own pick among the providers signed in. The
# user chooses the providers here, so no fixed model is one this harness can be
# sure to reach, and a judge that cannot answer refuses every command it is
# asked about. The judge then runs isolated from that configuration (below),
# so the model is read from it here, beforehand, and nothing else of it is.
# omp reads its own configuration — layered YAML — better than anything here
# could, so the role is asked of omp rather than parsed out of its files.
#
# Both named sources are files the judged agent can write, which is the gap a
# pinned model would close. The agent has sudo, so no file in the sandbox
# holds against a deliberate one (ADR 0090); what the tool-free run below does
# guarantee is that the judge does not run inside configuration, extensions or
# instructions the agent wrote. The trade is the one ADR 0127 §4 records for
# opencode.
SETTINGS="$HOME/.config/discobox/omp-harness.json"
AGENT_DIR="${PI_CODING_AGENT_DIR:-$HOME/.omp/agent}"

judge_model() {
	named=""
	if [ -f "$SETTINGS" ]; then
		named=$(jq -r '.judgeModel // "" | strings' "$SETTINGS" 2>/dev/null) || named=""
	fi
	if [ -z "$named" ]; then
		# A model selector is provider/model, one word; anything else omp
		# prints here — nothing, null, a message — names no model.
		named=$(timeout 60 omp config get modelRoles.default 2>/dev/null | tail -n 1 | tr -d '"') || named=""
		case "$named" in
		*/*) ;;
		*) named="" ;;
		esac
		case "$named" in
		*[[:space:]]*) named="" ;;
		esac
	fi
	printf '%s' "$named"
}

# The judge answers without extended thinking, which its verdict of a few
# dozen tokens has no use for. Set here rather than inherited, so neither the
# image's settings nor a caller decides how the gate thinks. --no-title: omp
# would otherwise name the session with a second model call.
set -- omp --print --no-session --no-title --thinking off
case "$model" in
judge)
	named=$(judge_model)
	if [ -n "$named" ]; then
		# One word, so no value in a file the agent can write becomes a flag
		# of its own on the judge's command line.
		set -- "$@" "--model=$named"
	fi
	;;
fast | "") ;;
*) set -- "$@" "--model=$model" ;;
esac
if [ -n "$system" ]; then
	set -- "$@" --system-prompt "$system"
fi

if [ -n "$no_tools" ]; then
	# The model answers from its prompt and executes nothing, and nothing the
	# caller can write reaches it. The caller is the agent this answer may be
	# judging, running as the same user in the same directory, so every source
	# omp would otherwise read is one it could have written: its global
	# configuration and a project's, an extension or hook (code in this
	# process), a skill or rule, an AGENTS.md found from the working
	# directory, a .env in any of the places omp reads one, and any PI_* or
	# OMP_* variable. claude-code's wrapper reaches the same place with
	# --restricted.
	#
	# So omp runs from an empty directory, under a home of its own, with an
	# agent directory holding only the credentials, every discovery off, and
	# no inherited PI_*/OMP_* variable beside the secrets the sandbox exports
	# and the image's own overlay — which is root-owned, and is the policy
	# baseline rather than anything the agent wrote.
	#
	# The credentials are omp's store, agent.db, and models.yml. The store
	# holds the OAuth sign-ins and is copied as a snapshot rather than a file:
	# omp writes it as it runs, and a copy torn between its pages is a judge
	# that cannot answer. models.yml is where an API key's environment
	# variable is named, and is carried for that; it is also where a custom
	# provider is configured, which is the trade ADR 0127 §4 records.
	isolated=$(mktemp -d)
	trap 'rm -rf "$isolated"' EXIT
	mkdir -p "$isolated/cwd" "$isolated/agent" "$isolated/home"
	if [ -f "$AGENT_DIR/agent.db" ]; then
		if command -v sqlite3 >/dev/null 2>&1; then
			sqlite3 "$AGENT_DIR/agent.db" ".backup '$isolated/agent/agent.db'" 2>/dev/null ||
				rm -f "$isolated/agent/agent.db"
		else
			cp "$AGENT_DIR/agent.db" "$isolated/agent/agent.db" 2>/dev/null || rm -f "$isolated/agent/agent.db"
		fi
	fi
	if [ -f "$AGENT_DIR/models.yml" ]; then
		cp "$AGENT_DIR/models.yml" "$isolated/agent/models.yml"
	fi
	overlay="${PI_CONFIG_FILES:-/etc/omp/discobox.yml}"
	for name in $(env | sed -n 's/^\(\(PI\|OMP\)_[A-Za-z0-9_]*\)=.*/\1/p'); do
		case "$name" in
		OMP_*_CREDENTIAL) ;;
		*) unset "$name" ;;
		esac
	done
	export HOME="$isolated/home" PI_CODING_AGENT_DIR="$isolated/agent" PI_CONFIG_FILES="$overlay" OMP_SKIP_SETUP=1
	cd "$isolated/cwd"
	set -- "$@" --no-tools --no-extensions --no-skills --no-rules --no-lsp --no-pty
fi

# `--` so a prompt that begins with a dash, an @, or a word omp has a command
# for is the prompt and not a flag, a file, or a command. Stdin is closed: in
# print mode omp reads whatever is piped to it into the prompt, and the judge's
# prompt is the one it was given.
set -- "$@" -- "$prompt"

if [ -n "$schema" ]; then
	# Captured rather than piped: a pipeline reports the exit status of its
	# last command, so omp failing would arrive as this script succeeding at
	# printing nothing.
	answer=$("$@" </dev/null) || exit
	printf '%s\n' "$answer" | discobox-prompt-answer
	exit
fi

# Not exec: an isolated directory is removed when omp exits.
"$@" </dev/null
