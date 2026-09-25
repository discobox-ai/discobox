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
#           JSON document conforming to it. `claude --print` says only what the
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
# "judge" sits in front of every credentialed command, so its latency is paid
# on each one; it takes the small model, which answers a short allow/deny well
# enough and fast enough not to stall the agent it is judging.
#
# The id rather than the `haiku` alias: the alias moves with the CLI, and the
# model a security gate runs on should change when someone decides it changes,
# not when an image is rebuilt. Bump it deliberately.
#
# The judge also answers without extended thinking, which Claude Code turns on
# by default. Its verdict is a few dozen tokens, and thinking before it was
# measured at 500 to 5,700 more: 5 to 50 seconds of a request held open,
# against about a second and a half without. Set here rather than inherited, so
# neither the image's environment nor a caller decides how the gate thinks.
case "$model" in
judge)
	model=claude-haiku-4-5
	export MAX_THINKING_TOKENS=0
	;;
fast | "") model=haiku ;;
esac

# A one-shot answer has no use for what a session does beside the model call —
# telemetry, error reporting, update and feature checks. Each run starts from a
# configuration home of its own, so none of it is ever cached, and it cost about
# two seconds of every ask.
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

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

	# And the run keeps what claude writes in a configuration home of its own:
	# the rewrites of .claude.json, the session transcript, the backups. A
	# judge answers asks in parallel, one claude per ask, and they must not
	# read-modify-write one file between them. The home starts with the
	# account and nothing else — .claude.json, which carries an API key, and
	# .credentials.json, which carries a login — and goes when claude exits.
	# What claude writes back to them is dropped with it: they hold sentinels,
	# and the control plane is what refreshes the credential behind one. A
	# run that is killed runs no trap; its TMPDIR, where this is made, is its
	# caller's to remove, which the sandbox agent does for a judge
	# (execs.RunOnce).
	account="${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json"
	login="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/.credentials.json"
	isolated=""
	trap 'if [ -n "$isolated" ]; then rm -rf "$isolated"; fi' EXIT
	isolated=$(mktemp -d)
	for file in "$account" "$login"; do
		if [ -f "$file" ]; then
			cp "$file" "$isolated/"
		fi
	done
	export CLAUDE_CONFIG_DIR="$isolated"
fi

if [ -n "$schema" ]; then
	# Captured rather than piped: a pipeline reports the exit status of its
	# last command, so claude failing would arrive as this script succeeding
	# at printing nothing.
	answer=$("$@") || exit
	printf '%s\n' "$answer" | discobox-prompt-answer
	exit
fi

# Not exec: an isolated home is removed when claude exits.
"$@"
