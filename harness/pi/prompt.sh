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
#           JSON document conforming to it. `pi -p` prints only what the model
#           said, but a model asked for JSON may still fence it, so a schema'd
#           answer goes through discobox-prompt-answer.
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

# The schema closes the prompt; the system text has a flag of its own.
composed="$prompt"
if [ -n "$schema" ]; then
	composed="$(printf '%s\n\n%s\n%s\n' "$composed" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi
# pi reads an argument starting with @ as a file to attach, `--` or not; one
# that leads with a space is sent as it is (ADR 26-10-15-259 §4).
case "$composed" in
@*) composed=" $composed" ;;
esac

SETTINGS="$HOME/.config/discobox/pi-harness.json"
PI_SETTINGS="$HOME/.pi/agent/settings.json"
CREDENTIALS="$HOME/.pi/agent/auth.json"

# The role, mapped onto a model pi can name. pi has no model roles, so "judge"
# and "fast" are one: `judgeModel` in Discobox's settings for this harness,
# else the default model in pi's own settings, else pi's own pick among the
# signed-in providers. The user chooses the providers here, so no pinned model
# is one this harness can be sure to reach, and a judge that cannot answer
# refuses every command it is asked about (ADR 26-10-15-259 §5).
named=""
case "$model" in
judge | fast)
	if [ -f "$SETTINGS" ]; then
		named=$(jq -r '.judgeModel // "" | strings' "$SETTINGS" 2>/dev/null) || named=""
	fi
	if [ -z "$named" ] && [ -f "$PI_SETTINGS" ]; then
		named=$(jq -r 'if (.defaultProvider | type) == "string" and (.defaultModel | type) == "string"
			and .defaultProvider != "" and .defaultModel != ""
			then "\(.defaultProvider)/\(.defaultModel)" else "" end' "$PI_SETTINGS" 2>/dev/null) || named=""
	fi
	;;
"") ;;
*) named="$model" ;;
esac

set -- pi -p --no-session
if [ -n "$named" ]; then
	# pi takes the word after --model as its value whatever it starts with, so
	# no value in a file the agent can write becomes a flag of its own on the
	# judge's command line. pi has no --model=NAME form.
	set -- "$@" --model "$named"
fi

# pi reads a value of its system-prompt flags as a file when a file by that
# name exists. A newline is added, so the system text is always the text.
nl='
'

# Set before anything is made, so an exit at any point below removes what was.
# A run that is killed instead runs no trap at all; its TMPDIR is its caller's
# to remove, which the sandbox agent does for a judge (execs.RunOnce).
isolated=""
trap 'if [ -n "$isolated" ]; then rm -rf "$isolated"; fi' EXIT

if [ -n "$no_tools" ]; then
	# The model answers from its prompt and executes nothing, and nothing the
	# caller can write reaches it. The caller is the agent this answer may be
	# judging, running as the same user in the same directory, so every source
	# pi would otherwise read is one it could have written: settings,
	# models.json (which can point a provider at another endpoint, or run a
	# command for its key), extensions (code in this process), skills, prompt
	# templates, a SYSTEM.md, a trusted project's .pi/, and context files —
	# AGENTS.md and its kin, which pi collects from the working directory up to
	# /, a repository root or not.
	#
	# So pi runs in a home of its own holding only the harness's credentials,
	# from an empty directory, with discovery off, and with no inherited
	# variable that would move its configuration or a provider.
	#
	# The credentials file is the agent's to write too, and a pi credential is
	# more than a secret: an API key beginning with ! is a command pi runs, an
	# API key's env object outranks the environment for its provider, and a
	# Copilot login carries the endpoint it is sent to, in its enterpriseUrl
	# and in the access token's own proxy-ep. Copied as found, one entry would
	# run the agent's code or point the judge at a server the agent runs. So
	# the judge's credentials are rebuilt from the fields that authenticate and
	# nothing else: an API key, or an OAuth login's access token, refresh token
	# (empty for OpenRouter's, whose access token is a key that does not
	# expire) and expiry. A key or token must be made of token characters — those of
	# keys, JWTs and base64url, and so of the sentinels minted in their shape —
	# which a command or a document never is. A Copilot login keeps its
	# refresh token alone, so the judge exchanges it for a Copilot token of its
	# own, at github.com, through the proxy.
	isolated=$(mktemp -d)
	mkdir -p "$isolated/home/.pi/agent" "$isolated/work"
	if [ -s "$CREDENTIALS" ]; then
		if ! jq 'def token: type == "string" and test("^[A-Za-z0-9_.~+/=:-]+$");
			with_entries(.key as $provider | .value as $c
			| if ($c | type) != "object" then empty
			  elif $c.type == "api_key" and ($c.key | token) then
				{key: $provider, value: {type: "api_key", key: $c.key}}
			  elif $c.type == "oauth" and $provider == "github-copilot" and ($c.refresh | token) then
				{key: $provider, value: {type: "oauth", access: "discobox-copilot-token-is-exchanged-in-the-sandbox", refresh: $c.refresh, expires: 0}}
			  elif $c.type == "oauth" and ($c.access | token) and (($c.refresh | token) or $c.refresh == "")
				and ($c.expires | type) == "number" then
				{key: $provider, value: {type: "oauth", access: $c.access, refresh: $c.refresh, expires: $c.expires}}
			  else empty end)' "$CREDENTIALS" >"$isolated/home/.pi/agent/auth.json" 2>/dev/null; then
			printf '%s\n' "discobox-prompt: pi's credentials could not be read" >&2
			exit 1
		fi
		chmod 0600 "$isolated/home/.pi/agent/auth.json"
	fi
	for name in $(env | sed -n 's/^\(PI_[A-Za-z0-9_]*\|XDG_[A-Za-z0-9_]*\|[A-Za-z0-9_]*_BASE_URL\)=.*/\1/p'); do
		unset "$name"
	done
	unset CLAUDE_CONFIG_DIR CODEX_HOME COPILOT_HOME
	export HOME="$isolated/home"
	cd "$isolated/work"
	set -- "$@" --no-tools --no-extensions --no-skills --no-prompt-templates --no-themes \
		--no-context-files --no-approve --offline
	if [ -n "$system" ]; then
		set -- "$@" --system-prompt "$system$nl"
	fi
elif [ -n "$system" ]; then
	# With tools, pi's own coding prompt is what tells the model how to use
	# them, so the caller's system text is added to it rather than replacing it.
	set -- "$@" --append-system-prompt "$system$nl"
fi

# stdin from /dev/null: given a stdin that is not a terminal, pi -p waits for
# it to close and reads it as more prompt, whatever its arguments say.
if [ -n "$schema" ]; then
	# Captured rather than piped: a pipeline reports the exit status of its
	# last command, so pi failing would arrive as this script succeeding at
	# printing nothing.
	answer=$("$@" -- "$composed" </dev/null) || exit
	printf '%s\n' "$answer" | discobox-prompt-answer
	exit
fi

# Not exec: the isolated directory is removed when pi exits.
"$@" -- "$composed" </dev/null
