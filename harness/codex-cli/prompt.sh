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
#           JSON document conforming to it. `codex exec` narrates its run, so
#           this prints the agent's last message alone (--output-last-message)
#           and, for a schema'd answer, the JSON document in it.
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

# Codex takes one prompt and has no separate system channel in exec mode, so the
# system text leads the prompt and the schema closes it.
composed="$prompt"
if [ -n "$system" ]; then
	composed="$(printf '%s\n\n%s\n' "$system" "$prompt")"
fi
if [ -n "$schema" ]; then
	composed="$(printf '%s\n\n%s\n%s\n' "$composed" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi

# The role, mapped onto a model this image can name.
#
# "judge" is the only thing standing between a granted credential and a command
# that misuses it, and it reads an argv written by the agent it is judging, so
# it is named rather than left to whatever the account happens to be configured
# with — a gate whose model is a user preference is not a gate. Luna is the
# small tier of its generation, since the judge's latency is paid on every
# credentialed command — the same choice the claude-code wrapper makes.
#
# The risk this accepts: an id Codex has retired fails the call, and a judge
# that cannot answer refuses the command. That is the safe direction, but it
# is a real outage — bump this when the model line moves.
#
# Anything else is left to the account's configured model.
set -- codex exec --skip-git-repo-check --color never
case "$model" in
judge) set -- "$@" --model gpt-6-luna ;;
fast | "") ;;
*) set -- "$@" --model "$model" ;;
esac

# Codex has no tools-off switch — read-only is the strictest sandboxing
# available, and approvals still have to be silenced or `exec` would stop and
# wait for one on a session with no one to answer it. Read access and
# read-only command execution are the residual this leaves (ADR 0090 §1); a
# judge here is not tool-free, only unable to write.
if [ -n "$no_tools" ]; then
	set -- "$@" --sandbox read-only --config approval_policy=never
fi

# Codex narrates its run on stdout, and the contract above is one answer and
# nothing else, so the last message is written to a file and that file is what
# is printed. A schema'd answer goes through discobox-prompt-answer, which
# prints the JSON document in it — codex may wrap an answer in a code fence,
# and a fence is framing, not an answer.
answer=$(mktemp)
trap 'rm -f "$answer"' EXIT
set -- "$@" --output-last-message "$answer"

# Not exec: the answer is printed once codex has written it.
"$@" "$composed" >/dev/null || exit
if [ -n "$schema" ]; then
	discobox-prompt-answer <"$answer"
else
	cat "$answer"
fi
