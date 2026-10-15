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
#           JSON document conforming to it. `omp -p` prints only what the model
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

# The egress proxy for omp's Anthropic transport, which reads no other proxy
# variable (proxy-env.sh); discobox-prompt does not run from a login shell.
if [ -r /etc/profile.d/omp-proxy.sh ]; then
	# shellcheck disable=SC1091 # Installed by the image (proxy-env.sh).
	. /etc/profile.d/omp-proxy.sh
fi

SETTINGS="$HOME/.config/discobox/omp-harness.json"
CREDENTIALS="$HOME/.config/discobox/omp-credentials.json"

# Set before anything is made, so an exit at any point below removes what was.
# A run that is killed instead runs no trap at all; its TMPDIR is its caller's
# to remove, which the sandbox agent does for a judge (execs.RunOnce).
isolated=""
trap 'if [ -n "$isolated" ]; then rm -rf "$isolated"; fi' EXIT
isolated=$(mktemp -d)

# role_model prints the model omp's settings give a role, following the
# `@role` aliases one role may name another by, or nothing. omp reads its
# settings for itself, so it is asked rather than its YAML parsed here, from an
# empty directory so no project's settings answer. Only the name is taken: the
# judge runs isolated from these settings, where an alias would mean nothing.
role_model() {
	roles=$(cd "$isolated" && omp config get modelRoles --json 2>/dev/null </dev/null) || return 0
	printf '%s' "$roles" | jq -r --arg role "$1" '
		.value as $roles
		| def resolve($name; $depth):
			($roles[$name] // "") as $model
			| if ($model | type) != "string" then ""
			  elif $depth > 8 then ""
			  elif $model == "*" then resolve("default"; $depth + 1)
			  elif ($model | startswith("@")) then resolve($model[1:]; $depth + 1)
			  else $model end;
		resolve($role; 0)
	' 2>/dev/null || true
}

# The role, mapped onto a model omp can name.
#
# "judge" is `judgeModel` in Discobox's settings for this harness, else omp's
# default model role, else omp's own pick among the signed-in providers. The
# user chooses the providers here, so no pinned model is one this harness can
# be sure to reach, and a judge that cannot answer refuses every command it is
# asked about (ADR 26-10-14-718 §5). omp's own `judge` role is not it: that
# role picks judgment-API models, a different kind.
#
# "fast" is omp's smol role, else the default one.
named=""
case "$model" in
judge)
	if [ -f "$SETTINGS" ]; then
		named=$(jq -r '.judgeModel // "" | strings' "$SETTINGS" 2>/dev/null) || named=""
	fi
	if [ -z "$named" ]; then
		named=$(role_model default)
	fi
	;;
fast)
	named=$(role_model smol)
	if [ -z "$named" ]; then
		named=$(role_model default)
	fi
	;;
"") ;;
*) named="$model" ;;
esac

set -- omp -p --no-session --no-title
if [ -n "$named" ]; then
	# One word, so no value in a file the agent can write becomes a flag of its
	# own on the judge's command line.
	set -- "$@" "--model=$named"
fi

# omp reads a one-line value of its system-prompt flags as a file path when a
# file by that name exists, and takes text with a newline in it as text. The
# newline is added so the system text is always the text.
nl='
'

if [ -n "$no_tools" ]; then
	# The model answers from its prompt and executes nothing, and nothing the
	# caller can write reaches it. The caller is the agent this answer may be
	# judging, running as the same user in the same directory, so every source
	# omp would otherwise read is one it could have written: settings, models.yml
	# (which can point a provider at another endpoint), a cached model catalog,
	# extensions (code in this process), skills, rules, a SYSTEM.md, .env files
	# in the working directory and the home, and context files — AGENTS.md and
	# its kin, which omp collects from the working directory up to a repository
	# root, or up to / when there is none, so a /tmp/AGENTS.md would reach a
	# judge run anywhere under /tmp.
	#
	# So omp runs in a home of its own holding only the harness's credentials,
	# imported from the file the harness config delivers, from an empty
	# directory that is its own repository root, with discovery off, and with no
	# inherited variable that would move its configuration or a provider.
	#
	# The credentials file is the agent's to write too, and an omp credential
	# is more than a secret: some carry the endpoint their requests go to
	# (github-copilot's apiEndpoint, snowflake's and alibaba-coding-plan's
	# enterpriseUrl, alibaba-token-plan's key, which is a JSON document with a
	# baseUrl in it). Imported as found, one entry would point the judge at a
	# server the agent runs. So the judge's credentials are rebuilt from the
	# fields that authenticate and name an account, and nothing else: an API
	# key, or an OAuth login's tokens, expiry, account, email and project. A
	# key or access token must be made of token characters — those of keys,
	# JWTs and base64url, and so of the sentinels minted in their shape, `_`
	# included — which a document never is: omp trims a key before it looks for one, so a check for a
	# leading brace alone would pass " {...}". A provider that needs an endpoint from its
	# credential is then out of the judge's reach, as a models.yml provider is.
	mkdir "$isolated/home" "$isolated/work" "$isolated/work/.git"
	if [ -s "$CREDENTIALS" ]; then
		if ! jq 'def token: type == "string" and test("^[A-Za-z0-9_.~+/=:-]+$");
			{version: 1, credentials: [.credentials[]?
			| select((.provider | type) == "string")
			| .provider as $provider | .credential as $c
			| if $c.type == "api_key" and ($c.key | token) then
				{provider: $provider, credential: {type: "api_key", key: $c.key}}
			  elif $c.type == "oauth" and ($c.access | token)
				and ($c.refresh | type) == "string" and ($c.expires | type) == "number" then
				{provider: $provider, credential: ({type: "oauth", access: $c.access, refresh: $c.refresh, expires: $c.expires}
					+ ($c | {accountId, email, projectId} | with_entries(select(.value | type == "string"))))}
			  else empty end]}' "$CREDENTIALS" >"$isolated/credentials.json" 2>/dev/null ||
			! HOME="$isolated/home" omp auth import "$isolated/credentials.json" >/dev/null 2>&1 </dev/null; then
			printf '%s\n' "discobox-prompt: omp could not import the harness's credentials" >&2
			exit 1
		fi
		rm -f "$isolated/credentials.json"
	fi
	for name in $(env | sed -n 's/^\(PI_[A-Za-z0-9_]*\|OMP_[A-Za-z0-9_]*\|XDG_[A-Za-z0-9_]*\|[A-Za-z0-9_]*_BASE_URL\)=.*/\1/p'); do
		unset "$name"
	done
	unset CLAUDE_CONFIG_DIR CODEX_HOME COPILOT_HOME
	# PI_PROXY went with the rest of PI_*, and omp's Anthropic transport reads
	# no other proxy variable (proxy-env.sh): the judge's follows the
	# HTTPS_PROXY every other request of its takes.
	if [ -n "${HTTPS_PROXY:-}" ]; then
		export PI_PROXY="$HTTPS_PROXY"
	fi
	export HOME="$isolated/home"
	cd "$isolated/work"
	set -- "$@" --no-tools --no-extensions --no-skills --no-rules --no-lsp
	if [ -n "$system" ]; then
		set -- "$@" "--system-prompt=$system$nl"
	fi
elif [ -n "$system" ]; then
	# With tools, omp's own coding prompt is what tells the model how to use
	# them, so the caller's system text is added to it rather than replacing it.
	set -- "$@" "--append-system-prompt=$system$nl"
fi

# stdin from /dev/null: given a stdin that is not a terminal, omp -p waits for
# it to close and reads it as more prompt, whatever its arguments say.
if [ -n "$schema" ]; then
	# Captured rather than piped: a pipeline reports the exit status of its
	# last command, so omp failing would arrive as this script succeeding at
	# printing nothing.
	answer=$("$@" -- "$composed" </dev/null) || exit
	printf '%s\n' "$answer" | discobox-prompt-answer
	exit
fi

# Not exec: the isolated directory is removed when omp exits.
"$@" -- "$composed" </dev/null
