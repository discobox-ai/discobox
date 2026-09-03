#!/bin/sh
set -eu

# discobox-prompt: the one-shot prompting interface an in-sandbox tool uses to
# ask this harness's model a question (ADR 0078). Its first consumer is the
# credential CLI's judge, which will not run a wrapped command until a model
# agrees the command is the use a human approved.
#
# Contract:
#   discobox-prompt --model ROLE --system TEXT --prompt TEXT --output-schema JSON [--no-tools]
#   stdout: the model's answer, and, when a schema is given, nothing but one
#           JSON document conforming to it.
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

# Anything left over is the prompt, so `discobox-prompt say something` works the
# way a person would expect at a shell.
if [ -z "$prompt" ] && [ $# -gt 0 ]; then
	prompt="$*"
fi
if [ -z "$prompt" ]; then
	printf '%s\n' "discobox-prompt: no prompt given" >&2
	exit 2
fi

# The role, mapped onto a model this image installed.
#
# "judge" is not a cheap classification, however short its answer is. It is the
# only thing standing between a granted credential and a command that misuses
# it, and it reads an argv written by the agent it is judging — so it takes the
# strongest model here, not the fastest. A wrong "allow" costs a credential;
# a wrong "deny" costs a retry.
#
# The id rather than the `sonnet` alias: the alias moves with the CLI, and the
# model a security gate runs on should change when someone decides it changes,
# not when an image is rebuilt. Bump it deliberately.
#
# "fast" keeps the small model, for callers that ask for speed and mean it.
case "$model" in
judge) model=claude-sonnet-5 ;;
fast | "") model=haiku ;;
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
if [ -n "$no_tools" ]; then
	# --tools "" is the model with nothing to call. --restricted also makes the
	# session ignore user, project and local settings files, so a caller this
	# CLI is judging cannot steer the judge through configuration it can itself
	# write; --disable-slash-commands is the same reasoning for skills. None of
	# the three costs the answer: the judge only ever needed the prompt.
	set -- "$@" --tools "" --restricted --disable-slash-commands
fi

exec "$@"
