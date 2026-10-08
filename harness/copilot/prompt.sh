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
#           JSON document conforming to it. `copilot -p --silent` prints only
#           what the model said, but a model asked for JSON may still fence
#           it, so a schema'd answer goes through discobox-prompt-answer.
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

# Copilot has no system-prompt flag in prompt mode, so the system text leads the
# prompt and the schema closes it.
composed="$prompt"
if [ -n "$system" ]; then
	composed="$(printf '%s\n\n%s\n' "$system" "$prompt")"
fi
if [ -n "$schema" ]; then
	composed="$(printf '%s\n\n%s\n%s\n' "$composed" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi

# --silent prints the answer without the usage summary; the rest keeps a
# one-shot run to the model call: no AGENTS.md or other custom instructions,
# no built-in GitHub MCP server, no ask_user tool waiting on nobody, and no
# update check.
set -- copilot --silent --no-custom-instructions --disable-builtin-mcps --no-ask-user --no-auto-update

# The role, mapped onto a model Copilot can name.
#
# "judge" is named rather than left to whatever the account happens to be
# configured with — a gate whose model is a user preference is not a gate. It
# is the model claude-code's judge was measured on (see that image's wrapper).
# Its reasoning is left at the model's default: Copilot checks an effort
# level against each model, and a level the model does not take would fail
# every judged command (ADR 26-10-02-840 §5).
#
# The risk this accepts: a plan or organization policy that does not offer the
# model fails the call, and a judge that cannot answer refuses the command.
# That is the safe direction, but it is a real outage — bump this when the
# model line moves.
#
# Anything else is left to the account's configured model.
case "$model" in
judge) set -- "$@" --model claude-sonnet-5.5 ;;
fast | "") ;;
*) set -- "$@" --model "$model" ;;
esac

# Set before anything is made, so an exit at any point below removes what was.
# A run that is killed instead runs no trap at all; its TMPDIR is its caller's
# to remove, which the sandbox agent does for a judge (execs.RunOnce).
isolated=""
trap 'if [ -n "$isolated" ]; then rm -rf "$isolated"; fi' EXIT
if [ -n "$no_tools" ]; then
	# An allowlist naming no tool that exists is the model with nothing to call.
	# An empty allowlist is not: Copilot reads it as no filter at all and offers
	# every tool.
	set -- "$@" --available-tools=discobox-no-tools

	# And the run keeps what Copilot writes in a COPILOT_HOME of its own — its
	# session state, its session store, its logs — run from an empty directory.
	# A judge answers asks in parallel, one copilot per ask, and they must not
	# share files one of them is writing; nor may the judge read settings,
	# plugins, MCP servers or hooks the judged agent could write, in its home
	# or in the directory it works in. The account needs no seeding: the token
	# is COPILOT_GITHUB_TOKEN, in the environment. COPILOT_ALLOW_ALL is dropped
	# so the empty directory is not trusted either.
	isolated=$(mktemp -d)
	mkdir "$isolated/home" "$isolated/work"
	export COPILOT_HOME="$isolated/home"
	unset COPILOT_ALLOW_ALL
	cd "$isolated/work"
fi

if [ -n "$schema" ]; then
	# Captured rather than piped: a pipeline reports the exit status of its
	# last command, so copilot failing would arrive as this script succeeding
	# at printing nothing.
	answer=$("$@" --prompt="$composed") || exit
	printf '%s\n' "$answer" | discobox-prompt-answer
	exit
fi

# Not exec: an isolated home is removed when copilot exits.
"$@" --prompt="$composed"
