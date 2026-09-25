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
#           JSON document conforming to it. `opencode run` frames its answer in
#           a transcript, so a schema'd ask is made with --format json and the
#           assistant's own text is taken from the event stream by structure
#           rather than found in prose: the prompt carries evidence written by
#           whatever is being judged, and anything that echoed it back into a
#           transcript would be indistinguishable from the answer.
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

# opencode run takes one message and no separate system channel, so the system
# text leads the prompt and the schema closes it.
composed="$prompt"
if [ -n "$system" ]; then
	composed="$(printf '%s\n\n%s\n' "$system" "$prompt")"
fi
if [ -n "$schema" ]; then
	composed="$(printf '%s\n\n%s\n%s\n' "$composed" \
		"Reply with one JSON document and nothing else: no prose, no code fence. It must validate against this JSON Schema:" \
		"$schema")"
fi

# The judge is the model named by `judgeModel` in Discobox's settings for this
# harness (.config/discobox/opencode-harness.json), else the model opencode
# itself starts with: the `model` setting in its configuration, else the last
# model picked with /models, else opencode's own pick among the connected
# providers. The user chooses the providers here, so no fixed model is one this
# harness can be sure to reach, and a judge that cannot answer refuses every
# command it is asked about. The judge then runs isolated from that
# configuration (below), so the model is read from it here, beforehand, and
# nothing else of it is.
#
# Both named sources are files the judged agent can write, which is the gap a
# pinned model would close: the agent could point the judge at another model,
# and with a provider key of its own in auth.json, at a provider it chose. The
# agent has sudo, so no file in the sandbox holds against a deliberate one
# (ADR 0090); what the tool-free run below does guarantee is that the judge
# does not run inside configuration, plugins or instructions the agent wrote.
# The choice is recorded in ADR 0127 §4.
SETTINGS="$HOME/.config/discobox/opencode-harness.json"
MODEL_PREFERENCE="${XDG_STATE_HOME:-$HOME/.local/state}/opencode/model.json"
# configured_model is opencode's `model` setting, read the way opencode layers
# its global configuration: config.json, then opencode.json, then
# opencode.jsonc, the later winning — so they are read in reverse, and the
# first that names a model is the one. opencode reads them as JSONC, so they
# are read the same way here: comments, line and block, and trailing commas
# are allowed. A file that still is not JSON is skipped, as one that is not
# there is. Only the model's name is taken from it.
configured_model() {
	dir="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
	for file in "$dir/opencode.jsonc" "$dir/opencode.json" "$dir/config.json"; do
		[ -f "$file" ] || continue
		configured=$(read_config_model "$file") || configured=""
		if [ -n "$configured" ]; then
			printf '%s' "$configured"
			return
		fi
	done
}

# read_config_model prints a JSONC file's top-level `model`, if it is a string.
# Comments and trailing commas are dropped outside strings, by walking the text
# rather than by pattern, so a URL or a "//" inside a string is left alone.
# node is what the image has for this; without it, plain JSON still reads.
read_config_model() {
	if ! command -v node >/dev/null 2>&1; then
		jq -r '.model // "" | strings' "$1" 2>/dev/null
		return
	fi
	node -e '
		// Two walks, each outside strings: comments out first, then trailing
		// commas, so a comment between a comma and its bracket cannot hide it.
		const walk = (src, visit) => {
			let out = "", i = 0, inString = false;
			while (i < src.length) {
				const c = src[i];
				if (inString) {
					out += c;
					if (c === "\\") { out += src[i + 1] ?? ""; i += 2; continue; }
					if (c === "\"") inString = false;
					i++;
					continue;
				}
				if (c === "\"") { inString = true; out += c; i++; continue; }
				const skip = visit(src, i);
				if (skip > 0) { i += skip; continue; }
				out += c;
				i++;
			}
			return out;
		};
		const noComments = walk(require("fs").readFileSync(process.argv[1], "utf8"), (src, i) => {
			if (src[i] === "/" && src[i + 1] === "/") { const end = src.indexOf("\n", i); return (end < 0 ? src.length : end) - i; }
			if (src[i] === "/" && src[i + 1] === "*") { const end = src.indexOf("*/", i + 2); return (end < 0 ? src.length : end + 2) - i; }
			return 0;
		});
		const json = walk(noComments, (src, i) => {
			if (src[i] !== ",") return 0;
			let j = i + 1;
			while (j < src.length && /\s/.test(src[j])) j++;
			return src[j] === "}" || src[j] === "]" ? 1 : 0;
		});
		const model = JSON.parse(json).model;
		if (typeof model === "string") process.stdout.write(model);
	' "$1" 2>/dev/null
}

judge_model() {
	named=""
	if [ -f "$SETTINGS" ]; then
		named=$(jq -r '.judgeModel // "" | strings' "$SETTINGS" 2>/dev/null) || named=""
	fi
	if [ -z "$named" ]; then
		named=$(configured_model)
	fi
	if [ -z "$named" ] && [ -f "$MODEL_PREFERENCE" ]; then
		named=$(jq -r '.recent[0] // empty | "\(.providerID)/\(.modelID)"' "$MODEL_PREFERENCE" 2>/dev/null) || named=""
	fi
	printf '%s' "$named"
}

# say_why repeats the stream's own error on stderr. Why a judge said nothing —
# a provider refusing the account, a model that is gone — is the one thing
# worth knowing when one will not answer, and stderr is where the agent logs it
# rather than handing it to whoever asked.
say_why() {
	failed=$(printf '%s\n' "$1" | jq -rs '
		[.[] | select(.type == "error") | .error.data.message // .error.name // "error"] | join("; ")
	' 2>/dev/null)
	if [ -n "$failed" ]; then
		printf '%s\n' "discobox-prompt: opencode answered nothing: $failed" >&2
	else
		printf '%s\n' "discobox-prompt: opencode answered nothing" >&2
	fi
}

# assistant_text prints what the model said, from `opencode run --format json`.
#
# The stream is one JSON event per line; a text part carries the model's own
# words in .part.text, and nothing a transcript wrapped around it can imitate,
# because this reads the field rather than the page.
#
# The parts go out one per line, each at its latest text, and the answer helper
# takes the last document among them. One per line matters twice: a part cannot
# be welded onto the answer's line, and a part carrying no document — a "done",
# a blank, whatever a future opencode adds after the answer — cannot silence it.
# Taking only the last part would do that, and a judge that answers nothing is
# every credential in the project refusing.
#
# What remains is that a document in a later part would win. Parts are ordered
# by when each first appeared, not by when its text was finalised, so "later"
# means a part that started later — a part that began before the answer and was
# updated after it still comes first. Nothing in the stream says which part is
# the model concluding, and ordering by the last update only moves which case
# loses, so the order is what there is.
#
# A stream that does not parse is a refusal rather than a reason to go looking
# in the text: that is either a broken run or an opencode whose format has
# changed, and neither is an answer. What the model said may still be fenced,
# so it goes through the image's answer helper.
assistant_text() {
	said=$(printf '%s\n' "$1" | jq -rs '
		reduce .[] as $event ({order: [], text: {}};
			if $event.type == "text" and ($event.part.text | type) == "string" then
				.text[$event.part.id] = $event.part.text
				| (if (.order | index($event.part.id)) == null then .order += [$event.part.id] else . end)
			else . end)
		| [.order[] as $id | .text[$id]] | join("\n")
	') || {
		printf '%s\n' "discobox-prompt: opencode's answer could not be read" >&2
		return 1
	}
	if [ -z "$said" ]; then
		say_why "$1"
		return 1
	fi
	printf '%s\n' "$said" | discobox-prompt-answer
}

set -- opencode run
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
*) set -- "$@" --model "$model" ;;
esac

if [ -n "$no_tools" ]; then
	# The model answers from its prompt and executes nothing, and nothing the
	# caller can write reaches it. The caller is the agent this answer may be
	# judging, running as the same user in the same directory, so every source
	# opencode would otherwise read is one it could have written: a config
	# file's permission rules (merged before ours, and the last matching rule
	# wins), a plugin (code in this process, whatever the permissions say), an
	# AGENTS.md or CLAUDE.md found from the working directory, a cached model
	# catalog pointing a provider somewhere else, and any OPENCODE_* variable.
	# claude-code's wrapper reaches the same place with --restricted.
	#
	# So opencode runs from an empty directory, with empty config, state and
	# cache directories, project configuration and Claude Code files off,
	# plugins off (--pure), and no inherited OPENCODE_* variable.
	#
	# Its data directory is its own too, holding only the credentials: the api
	# and oauth entries of auth.json. A `wellknown` entry is not a credential
	# but a URL whose /.well-known/opencode opencode fetches and merges in as
	# configuration — permission rules, providers, endpoints — past both
	# switches above, so it is the one part of that file left out.
	isolated=$(mktemp -d)
	trap 'rm -rf "$isolated"' EXIT
	mkdir -p "$isolated/cwd" "$isolated/config" "$isolated/state" "$isolated/cache" "$isolated/data/opencode"
	auth="${XDG_DATA_HOME:-$HOME/.local/share}/opencode/auth.json"
	if [ -f "$auth" ]; then
		jq 'with_entries(select(.value.type == "api" or .value.type == "oauth"))' "$auth" \
			>"$isolated/data/opencode/auth.json" 2>/dev/null || rm -f "$isolated/data/opencode/auth.json"
	fi
	for name in $(env | sed -n 's/^\(OPENCODE_[A-Za-z0-9_]*\)=.*/\1/p'); do
		unset "$name"
	done
	export XDG_CONFIG_HOME="$isolated/config" XDG_STATE_HOME="$isolated/state" XDG_CACHE_HOME="$isolated/cache" XDG_DATA_HOME="$isolated/data"
	export OPENCODE_DISABLE_PROJECT_CONFIG=1 OPENCODE_DISABLE_CLAUDE_CODE=1 OPENCODE_DISABLE_AUTOUPDATE=1
	# With no configuration left to merge into, one rule denying every
	# permission is the whole rule set.
	export OPENCODE_PERMISSION='{"*":"deny"}'
	cd "$isolated/cwd"
	# A session with no title gets one from a model call of its own, made
	# before the answer and with the judge's own model when no small model is
	# configured — which in this empty configuration none is. Measured, it was
	# a second round trip on every ask. The session is gone when the run is.
	set -- "$@" --pure --title=discobox-prompt
	# Not exec: the isolated directory is removed when opencode exits.
	if [ -n "$schema" ]; then
		# Captured rather than piped: a pipeline reports the exit status of
		# its last command, so opencode failing would arrive as this script
		# succeeding at printing nothing.
		if events=$("$@" --format json "$composed"); then
			assistant_text "$events" || exit
		else
			status=$?
			say_why "$events"
			exit "$status"
		fi
	else
		"$@" "$composed"
	fi
	exit
else
	# Nobody is there to answer a permission prompt.
	set -- "$@" --auto
fi

if [ -n "$schema" ]; then
	if events=$("$@" --format json "$composed"); then
		assistant_text "$events" || exit
	else
		# Read inside the else: after a compound `if` with no else branch, $?
		# is the compound's own status, which is zero — a failed run would
		# arrive as this script succeeding at printing nothing.
		status=$?
		say_why "$events"
		exit "$status"
	fi
	exit
fi
exec "$@" "$composed"
