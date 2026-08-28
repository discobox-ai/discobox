#!/bin/sh
set -eu

# discobox-prompt: the one-shot prompting interface an in-sandbox tool uses to
# ask this harness's model a question (ADR 0078). Its first consumer is the
# credential CLI's judge, which will not run a wrapped command until a model
# agrees the command is the use a human approved.
#
# Contract:
#   discobox-prompt --model ROLE --system TEXT --prompt TEXT --output-schema JSON
#   stdout: the model's answer, and, when a schema is given, nothing but one
#           JSON document conforming to it.
#   exit 0: the model answered. Anything else: it did not.
#
# --model names a role, never a model id: the caller does not know what this
# image installed. Mapping the role is this script's job.

model=""
system=""
prompt=""
schema=""

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
	--) shift; break ;;
	-*) printf '%s\n' "discobox-prompt: unknown flag $1" >&2; exit 2 ;;
	*) break ;;
	esac
done

# Anything left over is the prompt, so `discobox-prompt say something` works the
# way a person would expect at a shell.
if [ -z "$prompt" ] && [ $# -gt 0 ]; then
	prompt="$*"
fi
if [ -z "$prompt" ]; then
	printf '%s\n' "discobox-prompt: no prompt given" >&2
	exit 2
fi

# The role. "judge" is a short, mechanical classification, so it takes the
# smallest model here rather than whatever the session is configured with.
case "$model" in
judge | fast | "") model=haiku ;;
esac

# Claude Code has no schema flag, so the schema is stated as an instruction. It
# is appended last, after any caller system text, so it is the final word on the
# output shape.
if [ -n "$schema" ]; then
	system="$(printf '%s\n\n%s\n%s\n' "$system" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi

set -- claude --print "$prompt" --model "$model" --output-format text
if [ -n "$system" ]; then
	set -- "$@" --append-system-prompt "$system"
fi

exec "$@"
