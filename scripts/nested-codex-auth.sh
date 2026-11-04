#!/bin/sh
# Give a nested development server's Codex harness a local OpenAI API key or
# ChatGPT token. Inside a discobox either can be the outer proxy's sentinel: the
# nested proxy passes it through and the outer proxy swaps the real value in.
#
# Like nested-claude-auth.sh, it writes what saving a signed-in box's harness
# change would (nested-harness-login.sh): the secret bound to the harness
# config, and .codex/auth.json binding it, matching the harness's file delivery
# contract. ChatGPT account claims use the harness's normalizer; refresh stays
# with the outer control plane. Even subscription logins are plain token
# secrets here. Local Codex settings are not imported. --test verifies with
# codex exec in a box on the harness.
set -eu

HARNESS=codex
PROJECT=${DISCOBOX_PROJECT:-default}
AUTH_FILE="${CODEX_HOME:-$HOME/.codex}/auth.json"

TEST=0
for arg in "$@"; do
	case "$arg" in
	--test) TEST=1 ;;
	-h | --help)
		echo "usage: scripts/nested-codex-auth.sh [--test]"
		echo
		echo "Gives the local discobox server's Codex harness its local login."
		echo "Uses OPENAI_API_KEY from the environment, or an API key / ChatGPT token"
		echo "from \$CODEX_HOME/auth.json (~/.codex by default)."
		echo "DISCOBOX_SERVER (unix://) and DISCOBOX_PROJECT select the dev server."
		echo
		echo "  --test  prove the login works with codex exec in a nested sandbox"
		echo "          after saving it (costs one model round trip)"
		exit 0
		;;
	*)
		echo "nested-codex-auth: unknown argument $arg" >&2
		exit 2
		;;
	esac
done

die() {
	echo "nested-codex-auth: $*" >&2
	exit 1
}

command -v discobox >/dev/null || die "discobox is not on PATH (enter the dev shell, or go tool task build:cli)"
command -v jq >/dev/null || die "jq is required"
command -v curl >/dev/null || die "curl is required"

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/nested-harness-login.sh
. "$SCRIPT_DIR/nested-harness-login.sh"

# An explicit environment key wins; otherwise use the file's API key or its
# ChatGPT token set. Do not copy the raw auth file: it may contain a signed ID
# token and a refresh token. The harness normalizer builds its safe template.
TOKEN=${OPENAI_API_KEY:-}
if [ -z "$TOKEN" ] && [ -f "$AUTH_FILE" ]; then
	TOKEN=$(jq -er '.OPENAI_API_KEY | select(type == "string" and length > 0)' "$AUTH_FILE" 2>/dev/null) || TOKEN=""
fi
if [ -n "$TOKEN" ]; then
	OUTPUT=$(jq -nc --arg token "$TOKEN" '{
		secrets: [{envName: "OPENAI_API_KEY", name: "OpenAI API key", value: {token: $token}}],
		files: [{path: ".codex/auth.json",
			content: ({OPENAI_API_KEY: {"$secret": "OPENAI_API_KEY"}} | tojson)}]
	}')
	LOGIN_LABEL="OpenAI API key"
else
	[ -f "$AUTH_FILE" ] || die "found no OPENAI_API_KEY or Codex login in $AUTH_FILE"
	command -v node >/dev/null || die "node is required to import a ChatGPT login"
	OUTPUT=$(NESTED_CODEX_AUTH_FILE="$AUTH_FILE" NESTED_CODEX_HARNESS="$SCRIPT_DIR/../harness/codex-cli" node <<'NODE_EOF'
const fs = require('fs');
const path = require('path');
try {
  const dir = process.env.NESTED_CODEX_HARNESS;
  const { oauth, oauthAuthFile } = require(path.join(dir, 'codex-normalize.cjs'));
  const image = require(path.join(dir, 'image.json'));
  const auth = JSON.parse(fs.readFileSync(process.env.NESTED_CODEX_AUTH_FILE, 'utf8'));
  // No refresh credential is needed one level down, including for an outer
  // sentinel login whose refresh field is already this placeholder.
  const normalized = oauth({ ...auth, tokens: {
    ...auth.tokens, refresh_token: 'discobox-refresh-happens-in-the-control-plane',
  } }, image.harness.saveBack.providers.find(p => p.id === 'openai'));
  const value = { token: normalized.secret.token };
  if (normalized.secret.subscriptionType) value.subscriptionType = normalized.secret.subscriptionType;
  process.stdout.write(JSON.stringify({
    secrets: [{ envName: 'CODEX_OAUTH_TOKEN', name: 'Codex ChatGPT sign-in', value }],
    files: [oauthAuthFile(normalized.tokens)],
  }));
} catch {
  // Never include parsed credential data or a parser's source excerpt.
  process.stderr.write('nested-codex-auth: no usable Codex ChatGPT login in auth.json\n');
  process.exit(1);
}
NODE_EOF
	) || exit 1
	LOGIN_LABEL="Codex ChatGPT sign-in"
fi
echo "Using the local $LOGIN_LABEL."

apply_nested_login "$OUTPUT"
echo "$HARNESS now signs in with the local $LOGIN_LABEL."

[ "$TEST" = 1 ] || exit 0

# A box on the harness receives the login as every box will; codex exec in it
# goes out through the nested proxy.
echo "Verifying the $LOGIN_LABEL from a nested discobox…"
BOX=$(discobox admin box create --name nested-codex-auth-test --harness "$HARNESS" --wait --wait-timeout 10m -o json | jq -er .id) ||
	die "could not create a discobox on $HARNESS"
trap 'discobox admin box rm "$BOX" >/dev/null 2>&1 || true' EXIT
reply=$(discobox admin exec create "$BOX" -- timeout 180 codex exec 'Reply with exactly: discobox-ok' \
	--skip-git-repo-check --dangerously-bypass-approvals-and-sandbox </dev/null 2>&1) ||
	die "the $LOGIN_LABEL did not work: $(printf '%s' "$reply" | tail -n 5)"
echo "Codex replied: $(printf '%s' "$reply" | tail -n 1)"
