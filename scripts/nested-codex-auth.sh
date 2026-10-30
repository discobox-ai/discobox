#!/bin/sh
# Configure a nested development server's Codex harness with a local OpenAI
# API key or ChatGPT token. Inside a discobox either can be the outer proxy's sentinel: the
# nested proxy passes it through and the outer proxy swaps the real value in.
#
# Like nested-claude-auth.sh, drive the real configure/attach/commit flow so
# the server owns the secret, binding and configured state. Codex receives a
# templated .codex/auth.json, matching the harness's file delivery contract.
# ChatGPT account claims use the harness's normalizer; refresh stays with the
# outer control plane. Even subscription logins are plain token secrets here.
# Local Codex settings are not imported. --test verifies with codex exec.
set -eu

HARNESS=codex
HARNESS_FEATURE=codex-cli
PROJECT=${DISCOBOX_PROJECT:-default}
CONFIGURE_COMMAND=/usr/local/libexec/discobox/configure-codex
AUTH_FILE="${CODEX_HOME:-$HOME/.codex}/auth.json"

TEST=0
for arg in "$@"; do
	case "$arg" in
	--test) TEST=1 ;;
	-h | --help)
		echo "usage: scripts/nested-codex-auth.sh [--test]"
		echo
		echo "Configures the local discobox server's Codex harness with its local login."
		echo "Uses OPENAI_API_KEY from the environment, or an API key / ChatGPT token"
		echo "from \$CODEX_HOME/auth.json (~/.codex by default)."
		echo "DISCOBOX_SERVER (unix://) and DISCOBOX_PROJECT select the dev server."
		echo
		echo "  --test  prove the login works in an isolated Codex home in a nested"
		echo "          sandbox before configuring (costs one model round trip)"
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

# The configure endpoints have no CLI command of their own short of the
# interactive one, so they are called directly. Only a local socket is
# supported: a nested development server is always one.
SERVER=${DISCOBOX_SERVER:-$(discobox admin --help 2>&1 | sed -n 's/.*--server string.*(default "\([^"]*\)").*/\1/p')}
case "$SERVER" in
unix://*) SOCKET=${SERVER#unix://} ;;
*) die "only a unix:// server is supported, got '$SERVER'" ;;
esac
[ -S "$SOCKET" ] || die "no server socket at $SOCKET; is the nested server running (go tool task dev)?"

api() {
	method=$1
	path=$2
	curl -sS --unix-socket "$SOCKET" -X "$method" -w '\n%{http_code}' "http://discobox.local/projects/$PROJECT$path"
}

# api_ok runs api and prints the body, failing unless the status is 2xx.
api_ok() {
	response=$(api "$@")
	status=$(printf '%s' "$response" | tail -n1)
	body=$(printf '%s' "$response" | sed '$d')
	case "$status" in
	2??) printf '%s' "$body" ;;
	*) die "$1 $2: HTTP $status: $body" ;;
	esac
}

# An explicit environment key wins; otherwise use the file's API key or its
# ChatGPT token set. Do not copy the raw auth file: it may contain a signed ID
# token and a refresh token. The harness normalizer builds its safe template.
TOKEN=${OPENAI_API_KEY:-}
if [ -z "$TOKEN" ] && [ -f "$AUTH_FILE" ]; then
	TOKEN=$(jq -er '.OPENAI_API_KEY | select(type == "string" and length > 0)' "$AUTH_FILE" 2>/dev/null) || TOKEN=""
fi
if [ -n "$TOKEN" ]; then
	OUTPUT=$(jq -nc --arg token "$TOKEN" '{
		secrets: [{envName: "OPENAI_API_KEY", name: "OpenAI API key", type: "token", value: {token: $token}}],
		files: [{path: ".codex/auth.json", template: true,
			content: ({OPENAI_API_KEY: "{{ .secrets.OPENAI_API_KEY }}"} | tojson)}]
	}')
	LOGIN_LABEL="OpenAI API key"
else
	[ -f "$AUTH_FILE" ] || die "found no OPENAI_API_KEY or Codex login in $AUTH_FILE"
	command -v node >/dev/null || die "node is required to import a ChatGPT login"
	SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
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
    secrets: [{ envName: 'CODEX_OAUTH_TOKEN', name: 'Codex ChatGPT sign-in', type: 'token', value }],
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

# --- Drive the configure flow -----------------------------------------------

HARNESS_ID=$(discobox admin harness ls -o json | jq -er --arg slug "$HARNESS" '.harnessConfigs[] | select(.slug == $slug) | .id') ||
	die "the server has no $HARNESS harness"

# A flow left in flight by an earlier, interrupted run holds the harness; a new
# configure replaces it.
echo "Starting a configure discobox for $HARNESS ($HARNESS_ID)…"
SANDBOX_ID=$(api_ok POST "/harness-configs/$HARNESS_ID/configure" | jq -er .id)

# The same wait the CLI's configure does, observed the simplest way: the first
# exec that succeeds means the sandbox and its pool are up.
echo "Waiting for $SANDBOX_ID to be reachable…"
tries=0
until discobox admin exec create --user root "$SANDBOX_ID" -- true </dev/null >/dev/null 2>&1; do
	tries=$((tries + 1))
	[ "$tries" -lt 120 ] || die "$SANDBOX_ID never became reachable"
	sleep 2
done

# A box delivered its harness as a bundle has no configure command until the
# harness feature is provisioned, and provisioning it would then overwrite the
# replacement below. Wait for it: a box whose image came with the harness does
# not provision it, and passes at once. discobox-provision is itself platform
# content, so the exec fails until the platform's core is in place.
echo "Waiting for $SANDBOX_ID to provision $HARNESS…"
tries=0
until discobox admin exec create --user root "$SANDBOX_ID" -- discobox-provision wait "$HARNESS_FEATURE" </dev/null >/dev/null 2>&1; do
	tries=$((tries + 1))
	[ "$tries" -lt 300 ] || die "$SANDBOX_ID never provisioned $HARNESS"
	sleep 2
done

# Before the primary terminal is attached nothing has run, so the command it
# will launch can still be replaced. With --test the replacement first proves
# the sentinel works from a nested sandbox, as the interactive flow's own check
# does, and fails the flow — leaving the harness unconfigured — if it does not.
VERIFY=""
if [ "$TEST" = 1 ]; then
	VERIFY_AUTH=$(printf '%s' "$OUTPUT" | jq -c '. as $output | ($output.files[0].content | fromjson) |
		if .tokens then .tokens.access_token = $output.secrets[0].value.token
		else .OPENAI_API_KEY = $output.secrets[0].value.token end')
	VERIFY=$(cat <<EOF
echo "Verifying the $LOGIN_LABEL through the nested proxy…"
verify_home=\$(mktemp -d)
trap 'rm -rf "\$verify_home"' EXIT
cat >"\$verify_home/auth.json" <<'AUTH_EOF'
$VERIFY_AUTH
AUTH_EOF
if ! env -u CODEX_API_KEY -u OPENAI_API_KEY -u CODEX_ACCESS_TOKEN -u CODEX_OAUTH_TOKEN \\
	CODEX_HOME="\$verify_home" timeout 180 codex exec 'Reply with exactly: discobox-ok' \\
	--skip-git-repo-check --dangerously-bypass-approvals-and-sandbox \\
	-c 'cli_auth_credentials_store="file"' --output-last-message "\$verify_home/reply" \\
	>"\$verify_home/log" 2>&1; then
	echo "The $LOGIN_LABEL did not work:" >&2
	tail -n 5 "\$verify_home/log" >&2
	exit 1
fi
[ -s "\$verify_home/reply" ] || { echo "Codex returned no reply" >&2; exit 1; }
printf 'Codex replied: '
cat "\$verify_home/reply"
echo
EOF
	)
fi
REPLACEMENT=$(cat <<EOF
#!/bin/sh
set -eu
umask 077
$VERIFY
mkdir -p /run/discobox/configure
cat >/run/discobox/configure/harness-configure.json <<'OUTPUT_EOF'
$OUTPUT
OUTPUT_EOF
EOF
)
# Carried in the command line rather than on stdin, which an exec does not
# forward; base64 keeps it one inert word.
REPLACEMENT_B64=$(printf '%s\n' "$REPLACEMENT" | base64 | tr -d '\n')
discobox admin exec create --user root "$SANDBOX_ID" -- \
	sh -c "echo '$REPLACEMENT_B64' | base64 -d >'$CONFIGURE_COMMAND' && chmod 0755 '$CONFIGURE_COMMAND'" </dev/null >/dev/null

api_ok POST "/harness-configs/$HARNESS_ID/configure/attach" >/dev/null

# Attaching the primary terminal is what launches the configure command; it
# returns when the command exits.
discobox admin terminal attach "$SANDBOX_ID" primary </dev/null ||
	echo "nested-codex-auth: attach ended with an error; committing to read the real exit status" >&2

# Commit reads the exit status from the sandbox itself, and answers 409 while
# the command is still settling.
tries=0
while :; do
	response=$(api POST "/harness-configs/$HARNESS_ID/configure/commit")
	status=$(printf '%s' "$response" | tail -n1)
	body=$(printf '%s' "$response" | sed '$d')
	case "$status" in
	2??) break ;;
	409)
		case "$body" in
		*running*)
			tries=$((tries + 1))
			[ "$tries" -lt 60 ] || die "the configure command never finished"
			sleep 2
			continue
			;;
		esac
		;;
	esac
	die "commit: HTTP $status: $body"
done

printf '%s' "$body" | jq -e '.configured == true' >/dev/null || die "commit did not mark $HARNESS configured: $body"
echo "$HARNESS is configured with the local $LOGIN_LABEL."
