#!/bin/sh
set -eu

# discobox-prompt: the one-shot prompting interface an in-sandbox tool uses to
# ask this harness's model a question (ADR 0078). Its first consumer is the
# credential CLI's judge, which will not run a wrapped command until a model
# agrees the command is the use a human approved.
#
# Contract:
#   discobox-prompt --model ROLE --system TEXT --prompt TEXT --output-schema JSON
#   stdout: the model's answer, and, when a schema is given, one JSON document
#           conforming to it. `codex exec` frames its answer in a transcript,
#           so a caller parsing a schema'd answer must find the JSON in it.
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

# The role is left to the account's configured model unless the caller asked for
# something this image can name. Codex's model ids move with its releases, and a
# stale id here would fail every call rather than degrade to a slower answer.
set -- codex exec --skip-git-repo-check --color never
case "$model" in
judge | fast | "") ;;
*) set -- "$@" --model "$model" ;;
esac

exec "$@" "$composed"
