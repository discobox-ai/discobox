#!/bin/sh
# Configure a nested development instance's claude-code harness with the Claude
# Code login this sandbox is already running with, so `task dev` inside a
# discobox does not need its own /login.
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
# The harness is marked configured only by a configure flow that exited 0
# (CommitHarnessConfigConfigure), so this drives the real flow over the API and
# replaces the configure sandbox's configure command, before it launches, with
# one that writes the result the interactive flow would have. Everything after
# that — the secret, its binding and grant, the files, `configured` — is the
# server's normal apply. The sentinel is assumed to work; --test first proves it
# with `claude -p` through the nested proxy, which costs a model round trip.
#
# Run it against a nested server that is up (`go tool task dev`). Re-running it
# updates the harness's secret in place.
set -eu

HARNESS=claude-code
PROJECT=${DISCOBOX_PROJECT:-default}
CONFIGURE_COMMAND=/usr/local/libexec/discobox/configure-claude-code
CREDENTIALS_FILE="$HOME/.claude/.credentials.json"
CLAUDE_CONFIG_FILE="$HOME/.claude.json"
SETTINGS_FILE="$HOME/.claude/settings.json"
# See configure.sh: the delivered file's expiry is far out so Claude Code never
# tries to rotate a sentinel.
CREDENTIALS_EXPIRES_AT=4102444800000

TEST=0
for arg in "$@"; do
	case "$arg" in
	--test) TEST=1 ;;
	-h | --help)
		echo "usage: scripts/nested-claude-auth.sh [--test]"
		echo
		echo "Configures the local discobox server's $HARNESS harness with this"
		echo "sandbox's own Claude Code login (its sentinel). DISCOBOX_SERVER and"
		echo "DISCOBOX_PROJECT select the server and project as for the CLI."
		echo
		echo "  --test  prove the login works from a nested sandbox with claude -p"
		echo "          before configuring; without it the login is assumed to work"
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
case "$TOKEN" in
*"'"* | *"
"*) die "the $SECRET_ENV value holds a quote or newline; refusing to embed it in a shell script" ;;
esac
echo "Using this sandbox's $SECRET_LABEL ($SECRET_ENV)."

SETTINGS="null"
if [ -f "$SETTINGS_FILE" ]; then
	SETTINGS=$(jq -Rs . "$SETTINGS_FILE")
fi

# The result the interactive flow would write (configure.sh's write_output):
# the credential as a secret, settings.json as-is, and for a subscription login
# with scopes the credentials file as a template, so Claude Code sees the scopes
# (Remote Control needs user:profile) rather than an inference-only env token.
OUTPUT=$(jq -nc \
	--arg env "$SECRET_ENV" --arg label "$SECRET_LABEL" --arg token "$TOKEN" \
	--argjson settings "$SETTINGS" --argjson scopes "$SCOPES" --argjson subscription "$SUBSCRIPTION" \
	--argjson expires "$CREDENTIALS_EXPIRES_AT" '
	{
		secrets: [{envName: $env, name: $label, type: "token", value: {token: $token}}],
		files: (
			(if $settings then [{path: ".claude/settings.json", content: $settings}] else [] end)
			+ (if $env == "CLAUDE_CODE_OAUTH_TOKEN" and ($scopes | length) > 0 then [{
				path: ".claude/.credentials.json",
				template: true,
				content: ({claudeAiOauth: {
					accessToken: "{{ .secrets.\($env) }}",
					refreshToken: "discobox-refresh-happens-in-the-control-plane",
					expiresAt: $expires,
					scopes: $scopes,
					subscriptionType: $subscription
				}} | tojson)
			}] else [] end)
		)
	}')

# --- Drive the configure flow -----------------------------------------------

HARNESS_ID=$(discobox admin harnesses ls -o json | jq -er --arg slug "$HARNESS" '.harnessConfigs[] | select(.slug == $slug) | .id') ||
	die "the server has no $HARNESS harness"

# A flow left in flight by an earlier, interrupted run holds the harness; a new
# configure replaces it.
echo "Starting a configure discobox for $HARNESS ($HARNESS_ID)…"
SANDBOX_ID=$(api_ok POST "/harness-configs/$HARNESS_ID/configure" | jq -er .id)

# The same wait the CLI's configure does, observed the simplest way: the first
# exec that succeeds means the sandbox and its pool are up.
echo "Waiting for $SANDBOX_ID to be reachable…"
tries=0
until discobox admin exec --discobox-id "$SANDBOX_ID" create --user root -- true </dev/null >/dev/null 2>&1; do
	tries=$((tries + 1))
	[ "$tries" -lt 120 ] || die "$SANDBOX_ID never became reachable"
	sleep 2
done

# Before the primary terminal is attached nothing has run, so the command it
# will launch can still be replaced. With --test the replacement first proves
# the sentinel works from a nested sandbox, as the interactive flow's own check
# does, and fails the flow — leaving the harness unconfigured — if it does not.
VERIFY=""
if [ "$TEST" = 1 ]; then
	VERIFY=$(cat <<EOF
echo "Verifying the $SECRET_LABEL through the nested proxy…"
reply=\$(env -u ANTHROPIC_API_KEY -u CLAUDE_CODE_OAUTH_TOKEN $SECRET_ENV='$TOKEN' \\
	timeout 180 claude -p 'Reply with exactly: discobox-ok' 2>&1) || {
	echo "The credential did not work: \$reply" >&2
	exit 1
}
echo "Claude replied: \$reply"
EOF
	)
fi
REPLACEMENT=$(cat <<EOF
#!/bin/sh
set -eu
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
discobox admin exec --discobox-id "$SANDBOX_ID" create --user root -- \
	sh -c "echo '$REPLACEMENT_B64' | base64 -d >'$CONFIGURE_COMMAND' && chmod 0755 '$CONFIGURE_COMMAND'" </dev/null >/dev/null

api_ok POST "/harness-configs/$HARNESS_ID/configure/attach" >/dev/null

# Attaching the primary terminal is what launches the configure command; it
# returns when the command exits.
discobox admin terminal --discobox-id "$SANDBOX_ID" attach primary </dev/null ||
	echo "nested-claude-auth: attach ended with an error; committing to read the real exit status" >&2

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
echo "$HARNESS is configured with this sandbox's $SECRET_LABEL."
