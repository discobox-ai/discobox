#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# claude-code Definition). Collects one Anthropic credential and writes it to
# /run/discobox/harness-configure.json for discobox to apply to the HarnessConfig.
#
# Two credential shapes are supported:
#   - An Anthropic API key (ANTHROPIC_API_KEY), stored as a plain bearer secret.
#   - A Claude subscription login (CLAUDE_CODE_OAUTH_TOKEN). This runs `claude`
#     and asks the user to `/login`, which writes the rotating OAuth blob
#     (access token + refresh token + expiry) to ~/.claude/.credentials.json.
#     The whole blob, plus the fixed Anthropic token endpoint and client id, is
#     stored as an `oauth` secret so the control plane can refresh the access
#     token as it expires. We deliberately do NOT use `claude setup-token`: that
#     mints a single long-lived token with no refresh token, which cannot rotate.
#
# Reconfigure: /run/discobox/harness-previous-config.json lists the secrets a
# previous run stored, without their values. Each one's value is available as
# $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real credential on the
# way out, so the old credential can be exercised here without ever being
# readable in this sandbox. Keeping it is reported back as usePrevious, not as a
# value.
#
# Only the credential is captured. Claude Code's non-secret settings
# (.claude.json, .claude/settings.json) come from the image's declared harness
# files, so this flow never overrides them with a snapshot of this ephemeral
# sandbox's state.
set -eu

PREVIOUS_CONFIG=/run/discobox/harness-previous-config.json
OUTPUT=/run/discobox/harness-configure.json
CREDENTIALS_FILE="$HOME/.claude/.credentials.json"
CLAUDE_CONFIG_FILE="$HOME/.claude.json"

# The workspace the configure sandbox runs in. Unlike a run sandbox it has no
# source, so the image's .claude.json template trusts nothing — we mark the
# workspace trusted below so `claude /login` opens the login flow directly rather
# than stopping at the trust dialog.
WORKSPACE_DIR="${DISCOBOX_WORKING_ROOT:-/workspace}"

API_KEY_ENV=ANTHROPIC_API_KEY
OAUTH_ENV=CLAUDE_CODE_OAUTH_TOKEN

# Fixed Claude Code OAuth parameters. These are properties of the Claude Code
# public OAuth client, not of any one login, so they are baked into the harness
# rather than read from the credentials file. The control plane uses them to
# refresh the access token via grant_type=refresh_token.
OAUTH_TOKEN_URL="https://console.anthropic.com/v1/oauth/token"
OAUTH_CLIENT_ID="9d1c250a-e61b-44d9-88ed-5944d1962f5e"

# Holds the collected OAuth payload JSON between collection and write_output.
OAUTH_PAYLOAD_FILE=""

# ensure_workspace_trusted marks the workspace (and the current directory) as
# already trusted in ~/.claude.json, so an interactive `claude` launch reaches
# its task — here, /login — without stopping at the trust dialog first. It merges
# into the file the image installed rather than replacing it, and touches only
# trust/onboarding, never a credential.
ensure_workspace_trusted() {
	CLAUDE_CONFIGURE_CONFIG_FILE="$CLAUDE_CONFIG_FILE" \
		CLAUDE_CONFIGURE_TRUST_DIRS="$WORKSPACE_DIR
$PWD" node <<-'NODE_EOF'
		const fs = require('fs');
		const nodePath = require('path');
		const file = process.env.CLAUDE_CONFIGURE_CONFIG_FILE;
		let config = {};
		try {
			config = JSON.parse(fs.readFileSync(file, 'utf8')) || {};
		} catch (err) {
			config = {};
		}
		config.hasCompletedOnboarding = true;
		config.projects = config.projects || {};
		for (const dir of process.env.CLAUDE_CONFIGURE_TRUST_DIRS.split('\n')) {
			const trimmed = (dir || '').trim();
			if (!trimmed) continue;
			config.projects[trimmed] = Object.assign({}, config.projects[trimmed], { hasTrustDialogAccepted: true });
		}
		fs.mkdirSync(nodePath.dirname(file), { recursive: true });
		fs.writeFileSync(file, JSON.stringify(config, null, 2));
	NODE_EOF
}

# env_label prints the human name recorded alongside the secret.
env_label() {
	if [ "$1" = "$API_KEY_ENV" ]; then
		echo "Anthropic API key"
	else
		echo "Claude Code subscription login"
	fi
}

# previous_env prints the env name of the credential a previous configure run
# stored, or nothing. Only names whose PREV_ variable is actually set count: a
# seeded secret with no value behind it cannot be reused.
previous_env() {
	[ -f "$PREVIOUS_CONFIG" ] || return 0
	CLAUDE_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.CLAUDE_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const secrets = previous.secrets || [];
		for (const envName of ['CLAUDE_CODE_OAUTH_TOKEN', 'ANTHROPIC_API_KEY']) {
			const known = secrets.some((s) => s && s.envName === envName);
			if (known && process.env['PREV_' + envName]) {
				process.stdout.write(envName + '\n');
				break;
			}
		}
	NODE_EOF
}

# verify_credential runs a trivial non-interactive prompt with only the chosen
# credential in the environment, so the check exercises exactly what a real
# sandbox will run with. The interactive login writes a credentials file that
# would otherwise mask a bad token, so it is moved aside for the duration.
verify_credential() {
	verify_env=$1
	verify_token=$2
	verify_stashed=""
	if [ -f "$CREDENTIALS_FILE" ]; then
		verify_stashed="$CREDENTIALS_FILE.discobox-configure"
		mv "$CREDENTIALS_FILE" "$verify_stashed"
	fi

	set +e
	if [ "$verify_env" = "$API_KEY_ENV" ]; then
		verify_output=$(env -u "$OAUTH_ENV" "$API_KEY_ENV=$verify_token" \
			timeout 180 claude -p 'Reply with exactly: discobox-ok' 2>&1)
	else
		verify_output=$(env -u "$API_KEY_ENV" "$OAUTH_ENV=$verify_token" \
			timeout 180 claude -p 'Reply with exactly: discobox-ok' 2>&1)
	fi
	verify_status=$?
	set -e

	if [ -n "$verify_stashed" ]; then
		mv "$verify_stashed" "$CREDENTIALS_FILE"
	fi

	if [ "$verify_status" -ne 0 ] || [ -z "$verify_output" ]; then
		echo
		echo "The credential did not work:" >&2
		echo "$verify_output" >&2
		return 1
	fi
	echo "Claude replied: $verify_output"
	return 0
}

# collect_api_key reads an API key without echoing it.
collect_api_key() {
	printf 'Enter your Anthropic API key: ' >&2
	stty -echo 2>/dev/null || true
	read -r collected_token
	stty echo 2>/dev/null || true
	echo >&2
	[ -n "$collected_token" ] || return 1
	echo "$collected_token"
}

# collect_oauth_login drives the interactive Claude Code login. There is no
# headless login command, so it runs `claude /login` — equivalent to starting
# claude and typing /login — which opens the browser login and writes
# ~/.claude/.credentials.json with the rotating OAuth blob. The workspace is
# pre-trusted (ensure_workspace_trusted) so this reaches the login directly. It
# records the full blob (token endpoint and client id included) to
# $OAUTH_PAYLOAD_FILE and echoes just the access token for verification.
# $OAUTH_PAYLOAD_FILE must be set by the caller (a fixed path so the write
# survives this function's command substitution).
collect_oauth_login() {
	echo "Starting the Claude subscription login." >&2
	echo "Complete the browser login, then leave Claude Code (type /exit or press Ctrl-D)." >&2
	echo >&2

	# Capture only this fresh login: move any existing credential aside first.
	login_stashed=""
	if [ -f "$CREDENTIALS_FILE" ]; then
		login_stashed="$CREDENTIALS_FILE.discobox-login"
		mv "$CREDENTIALS_FILE" "$login_stashed"
	fi

	# claude needs a real TTY for its interactive UI; run it under script. Passing
	# /login as the argument enters the login flow just as typing it would.
	set +e
	script -q -e -c 'claude /login' /dev/null >&2
	set -e

	if [ ! -f "$CREDENTIALS_FILE" ]; then
		[ -n "$login_stashed" ] && mv "$login_stashed" "$CREDENTIALS_FILE"
		echo "No credentials were written; did you complete /login?" >&2
		return 1
	fi

	collected_payload=$(CLAUDE_CONFIGURE_CREDENTIALS_FILE="$CREDENTIALS_FILE" \
		CLAUDE_CONFIGURE_TOKEN_URL="$OAUTH_TOKEN_URL" \
		CLAUDE_CONFIGURE_CLIENT_ID="$OAUTH_CLIENT_ID" node <<-'NODE_EOF'
		const fs = require('fs');
		let oauth = {};
		try {
			oauth = (JSON.parse(fs.readFileSync(process.env.CLAUDE_CONFIGURE_CREDENTIALS_FILE, 'utf8')) || {}).claudeAiOauth || {};
		} catch (err) {
			oauth = {};
		}
		if (typeof oauth.accessToken !== 'string' || !oauth.accessToken ||
			typeof oauth.refreshToken !== 'string' || !oauth.refreshToken) {
			process.exit(3);
		}
		const out = {
			token: oauth.accessToken,
			refreshToken: oauth.refreshToken,
			tokenUrl: process.env.CLAUDE_CONFIGURE_TOKEN_URL,
			clientId: process.env.CLAUDE_CONFIGURE_CLIENT_ID,
		};
		if (typeof oauth.expiresAt === 'number') {
			out.accessTokenExpiresAt = oauth.expiresAt;
		}
		process.stdout.write(JSON.stringify(out));
	NODE_EOF
	)
	if [ -z "$collected_payload" ]; then
		echo "Could not read a subscription credential from the login." >&2
		echo "A refresh token is required; use an API key instead if login is unavailable." >&2
		return 1
	fi

	printf '%s' "$collected_payload" > "$OAUTH_PAYLOAD_FILE"
	# Echo only the access token so the caller can verify it like any bearer.
	CLAUDE_CONFIGURE_PAYLOAD="$collected_payload" node -e \
		'process.stdout.write(JSON.parse(process.env.CLAUDE_CONFIGURE_PAYLOAD).token)'
}

# write_output records the result. Shapes:
#   - keep previous:  usePrevious marker, no value (KEEP_PREVIOUS set).
#   - oauth login:    the full rotating blob from $OAUTH_PAYLOAD_FILE, type oauth.
#   - api key:        a plain bearer { token }.
write_output() {
	write_env=$1
	mkdir -p /run/discobox
	CLAUDE_CONFIGURE_ENV_NAME="$write_env" \
		CLAUDE_CONFIGURE_NAME="$(env_label "$write_env")" \
		CLAUDE_CONFIGURE_TYPE="${OUTPUT_TYPE:-bearer}" \
		CLAUDE_CONFIGURE_TOKEN="${OUTPUT_TOKEN:-}" \
		CLAUDE_CONFIGURE_KEEP_PREVIOUS="${KEEP_PREVIOUS:-}" \
		CLAUDE_CONFIGURE_PAYLOAD_FILE="${OAUTH_PAYLOAD_FILE:-}" \
		CLAUDE_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const secret = {
			envName: process.env.CLAUDE_CONFIGURE_ENV_NAME,
			name: process.env.CLAUDE_CONFIGURE_NAME,
			type: process.env.CLAUDE_CONFIGURE_TYPE || 'bearer',
		};
		if (process.env.CLAUDE_CONFIGURE_KEEP_PREVIOUS) {
			secret.usePrevious = true;
		} else if (secret.type === 'oauth') {
			secret.value = JSON.parse(fs.readFileSync(process.env.CLAUDE_CONFIGURE_PAYLOAD_FILE, 'utf8'));
		} else {
			secret.value = { token: process.env.CLAUDE_CONFIGURE_TOKEN };
		}
		fs.writeFileSync(process.env.CLAUDE_CONFIGURE_OUTPUT, JSON.stringify({ files: [], secrets: [secret] }));
	NODE_EOF
}

PREVIOUS_ENV=$(previous_env)

# Trust the workspace up front so every claude invocation below — the /login TUI
# and the `claude -p` verification — runs without a trust prompt.
ensure_workspace_trusted

ENV_NAME=""
TOKEN=""
OUTPUT_TYPE="bearer"
KEEP_PREVIOUS=""
while [ -z "$ENV_NAME" ]; do
	echo "How should Claude Code authenticate?"
	echo "  1) Anthropic API key"
	echo "  2) Sign in with a Claude subscription (rotating login)"
	if [ -n "$PREVIOUS_ENV" ]; then
		echo "  3) Keep the existing credential ($(env_label "$PREVIOUS_ENV"))"
	fi
	printf 'Choose [1]: '
	# End of input means nobody is there to answer: stop rather than spin.
	if ! read -r choice; then
		echo >&2
		echo "No input; aborting without configuring Claude Code." >&2
		exit 1
	fi
	echo
	case "${choice:-1}" in
	1)
		ENV_NAME="$API_KEY_ENV"
		OUTPUT_TYPE="bearer"
		TOKEN=$(collect_api_key) || {
			echo "An API key is required." >&2
			ENV_NAME=""
			continue
		}
		;;
	2)
		ENV_NAME="$OAUTH_ENV"
		OUTPUT_TYPE="oauth"
		OAUTH_PAYLOAD_FILE=$(mktemp)
		TOKEN=$(OAUTH_PAYLOAD_FILE="$OAUTH_PAYLOAD_FILE" collect_oauth_login) || {
			rm -f "$OAUTH_PAYLOAD_FILE"
			OAUTH_PAYLOAD_FILE=""
			ENV_NAME=""
			continue
		}
		;;
	3)
		if [ -z "$PREVIOUS_ENV" ]; then
			echo "Choose 1 or 2." >&2
			continue
		fi
		# The sentinel, not the credential: good enough to verify with, and
		# worthless if it leaks.
		ENV_NAME="$PREVIOUS_ENV"
		KEEP_PREVIOUS=yes
		eval "TOKEN=\${PREV_$PREVIOUS_ENV}"
		;;
	*)
		echo "Choose one of the listed options." >&2
		continue
		;;
	esac

	echo "Checking the credential with a test prompt…"
	if ! verify_credential "$ENV_NAME" "$TOKEN"; then
		echo
		echo "Let's try again."
		echo
		ENV_NAME=""
		TOKEN=""
		OUTPUT_TYPE="bearer"
		KEEP_PREVIOUS=""
		[ -n "$OAUTH_PAYLOAD_FILE" ] && rm -f "$OAUTH_PAYLOAD_FILE"
		OAUTH_PAYLOAD_FILE=""
	fi
done

if [ -n "$KEEP_PREVIOUS" ]; then
	OUTPUT_TOKEN="" write_output "$ENV_NAME"
else
	OUTPUT_TOKEN="$TOKEN" write_output "$ENV_NAME"
fi

[ -n "$OAUTH_PAYLOAD_FILE" ] && rm -f "$OAUTH_PAYLOAD_FILE"

echo
echo "Claude Code configuration complete."
