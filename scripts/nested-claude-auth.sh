#!/bin/sh
# Give a nested development instance's claude-code harness the Claude Code
# login this sandbox is already running with, so `task dev` inside a discobox
# does not need its own /login.
#
# Inside a discobox, ~/.claude/.credentials.json (or ~/.claude.json's
# primaryApiKey) holds the *outer* instance's sentinel, not a credential. That
# sentinel is still usable one level down: the nested pool proxy passes a string
# it does not know through untouched, forwards through this sandbox's
# HTTPS_PROXY (proxy/upstream.go), and the outer proxy swaps it for the real
# token. So the nested harness only has to hand its sandboxes the outer sentinel.
#
# It is stored as a plain `token` secret even for a subscription login. An
# `oauth` secret would have the nested control plane refresh it on a 401 using a
# refresh token it does not have; the outer control plane already keeps the
# real token fresh behind the sentinel.
#
# What it writes is what saving a signed-in box's harness change would: the
# secret bound to the harness config, settings.json, and for a subscription
# login with scopes the credentials file, so Claude Code sees the scopes
# (Remote Control needs user:profile) rather than an inference-only env token.
# See nested-harness-login.sh. The sentinel is assumed to work; --test then
# proves it with `claude -p` in a box on the harness, which costs a model round
# trip.
#
# Run it against a nested server that is up (`go tool task dev`). Re-running it
# updates the harness's secret in place.
set -eu

HARNESS=claude-code
PROJECT=${DISCOBOX_PROJECT:-default}
CREDENTIALS_FILE="$HOME/.claude/.credentials.json"
CLAUDE_CONFIG_FILE="$HOME/.claude.json"
SETTINGS_FILE="$HOME/.claude/settings.json"

TEST=0
for arg in "$@"; do
	case "$arg" in
	--test) TEST=1 ;;
	-h | --help)
		echo "usage: scripts/nested-claude-auth.sh [--test]"
		echo
		echo "Gives the local discobox server's $HARNESS harness this"
		echo "sandbox's own Claude Code login (its sentinel). DISCOBOX_SERVER and"
		echo "DISCOBOX_PROJECT select the server and project as for the CLI."
		echo
		echo "  --test  prove the login works from a nested sandbox with claude -p"
		echo "          after saving it; without it the login is assumed to work"
		exit 0
		;;
	*)
		echo "nested-claude-auth: unknown argument $arg" >&2
		exit 2
		;;
	esac
done

die() {
	echo "nested-claude-auth: $*" >&2
	exit 1
}

command -v discobox >/dev/null || die "discobox is not on PATH (enter the dev shell, or go tool task build:cli)"
command -v jq >/dev/null || die "jq is required"
command -v curl >/dev/null || die "curl is required"

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/nested-harness-login.sh
. "$SCRIPT_DIR/nested-harness-login.sh"

# --- What this sandbox is signed in with ------------------------------------

SECRET_ENV=""
SECRET_LABEL=""
TOKEN=""
SCOPES="null"
SUBSCRIPTION="null"
if [ -f "$CREDENTIALS_FILE" ] && TOKEN=$(jq -er '.claudeAiOauth.accessToken // empty' "$CREDENTIALS_FILE" 2>/dev/null); then
	SECRET_ENV=CLAUDE_CODE_OAUTH_TOKEN
	SECRET_LABEL="Claude Code subscription login"
	SCOPES=$(jq -c '.claudeAiOauth.scopes // null' "$CREDENTIALS_FILE")
	SUBSCRIPTION=$(jq -c '.claudeAiOauth.subscriptionType // null' "$CREDENTIALS_FILE")
elif [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
	SECRET_ENV=CLAUDE_CODE_OAUTH_TOKEN
	SECRET_LABEL="Claude Code subscription login"
	TOKEN=$CLAUDE_CODE_OAUTH_TOKEN
elif [ -f "$CLAUDE_CONFIG_FILE" ] && TOKEN=$(jq -er '.primaryApiKey // empty' "$CLAUDE_CONFIG_FILE" 2>/dev/null); then
	SECRET_ENV=ANTHROPIC_API_KEY
	SECRET_LABEL="Anthropic API key"
elif [ -n "${ANTHROPIC_API_KEY:-}" ]; then
	SECRET_ENV=ANTHROPIC_API_KEY
	SECRET_LABEL="Anthropic API key"
	TOKEN=$ANTHROPIC_API_KEY
else
	die "found no Claude Code login in $CREDENTIALS_FILE, $CLAUDE_CONFIG_FILE, or the environment"
fi
echo "Using this sandbox's $SECRET_LABEL ($SECRET_ENV)."

SETTINGS="null"
if [ -f "$SETTINGS_FILE" ]; then
	SETTINGS=$(jq -Rs . "$SETTINGS_FILE")
fi

# The secret, settings.json as-is, and for a subscription login with scopes the
# credentials file the harness's own normalizer writes, its token a binding to
# the secret's variable.
CREDENTIALS_FILE_JSON=null
if [ "$SECRET_ENV" = CLAUDE_CODE_OAUTH_TOKEN ] && [ "$(printf '%s' "$SCOPES" | jq 'length')" -gt 0 ]; then
	command -v node >/dev/null || die "node is required to write the credentials file"
	CREDENTIALS_FILE_JSON=$(NESTED_SCOPES="$SCOPES" NESTED_SUBSCRIPTION="$SUBSCRIPTION" \
		NESTED_HARNESS="$SCRIPT_DIR/../harness/claude-code" node -e '
const path = require("path");
const { credentialsFile } = require(path.join(process.env.NESTED_HARNESS, "claude-code-normalize.cjs"));
process.stdout.write(JSON.stringify(credentialsFile({
  scopes: JSON.parse(process.env.NESTED_SCOPES),
  subscriptionType: JSON.parse(process.env.NESTED_SUBSCRIPTION) ?? undefined,
})));
') || die "could not write the credentials file"
fi
OUTPUT=$(jq -nc \
	--arg env "$SECRET_ENV" --arg label "$SECRET_LABEL" --arg token "$TOKEN" \
	--argjson settings "$SETTINGS" --argjson credentials "$CREDENTIALS_FILE_JSON" '
	{
		secrets: [{envName: $env, name: $label, value: {token: $token}}],
		files: (
			(if $settings then [{path: ".claude/settings.json", content: $settings}] else [] end)
			+ (if $credentials then [$credentials] else [] end)
		)
	}')

apply_nested_login "$OUTPUT"
echo "$HARNESS now signs in with this sandbox's $SECRET_LABEL."

[ "$TEST" = 1 ] || exit 0

# A box on the harness receives the login as every box will; claude -p in it
# goes out through the nested proxy.
echo "Verifying the $SECRET_LABEL from a nested discobox…"
BOX=$(discobox admin box create --name nested-claude-auth-test --harness "$HARNESS" --wait --wait-timeout 10m -o json | jq -er .id) ||
	die "could not create a discobox on $HARNESS"
trap 'discobox admin box rm "$BOX" >/dev/null 2>&1 || true' EXIT
reply=$(discobox admin exec create "$BOX" -- timeout 180 claude -p 'Reply with exactly: discobox-ok' </dev/null 2>&1) ||
	die "the credential did not work: $reply"
echo "Claude replied: $reply"
