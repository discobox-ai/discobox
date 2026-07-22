#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# claude-code Definition). Collects one Anthropic credential — an API key or a
# subscription token minted by `claude setup-token` — verifies it actually works
# with `claude -p`, and writes it to /run/discobox/harness-configure.json for
# discobox to apply to the HarnessConfig.
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

API_KEY_ENV=ANTHROPIC_API_KEY
OAUTH_ENV=CLAUDE_CODE_OAUTH_TOKEN

# env_label prints the human name recorded alongside the secret.
env_label() {
	if [ "$1" = "$API_KEY_ENV" ]; then
		echo "Anthropic API key"
	else
		echo "Claude Code OAuth token"
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

# collect_setup_token runs `claude setup-token` interactively and recovers the
# token it prints. The command drives a browser login and needs a TTY, so it is
# recorded through `script` rather than piped, then scraped from the transcript.
collect_setup_token() {
	transcript=$(mktemp)
	echo "Starting the Claude web login. Open the printed URL, then paste the code back here." >&2
	echo >&2
	set +e
	script -q -e -c 'claude setup-token' "$transcript" >&2
	setup_status=$?
	set -e
	if [ "$setup_status" -ne 0 ]; then
		rm -f "$transcript"
		echo "claude setup-token failed." >&2
		return 1
	fi
	# Strip the escape sequences the TUI emitted, then take the last token it
	# printed. Fall back to the credentials file the login also writes.
	collected_token=$(sed 's/\x1b\[[0-9;?]*[A-Za-z]//g; s/\r//g' "$transcript" |
		grep -o 'sk-ant-[A-Za-z0-9_-]\{20,\}' | tail -n 1 || true)
	rm -f "$transcript"
	if [ -z "$collected_token" ] && [ -f "$CREDENTIALS_FILE" ]; then
		collected_token=$(CLAUDE_CONFIGURE_CREDENTIALS_FILE="$CREDENTIALS_FILE" node <<-'NODE_EOF'
			const fs = require('fs');
			let oauth = {};
			try {
				oauth = (JSON.parse(fs.readFileSync(process.env.CLAUDE_CONFIGURE_CREDENTIALS_FILE, 'utf8')) || {}).claudeAiOauth || {};
			} catch (err) {
				oauth = {};
			}
			process.stdout.write(typeof oauth.accessToken === 'string' ? oauth.accessToken : '');
		NODE_EOF
		)
	fi
	[ -n "$collected_token" ] || {
		echo "Could not read a token from claude setup-token." >&2
		return 1
	}
	echo "$collected_token"
}

# write_output records the result: either the credential just collected, or a
# usePrevious marker that keeps the secret the control plane already holds.
write_output() {
	mkdir -p /run/discobox
	CLAUDE_CONFIGURE_ENV_NAME="$1" \
		CLAUDE_CONFIGURE_NAME="$(env_label "$1")" \
		CLAUDE_CONFIGURE_TOKEN="${2:-}" \
		CLAUDE_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const token = process.env.CLAUDE_CONFIGURE_TOKEN;
		const secret = {
		  envName: process.env.CLAUDE_CONFIGURE_ENV_NAME,
		  name: process.env.CLAUDE_CONFIGURE_NAME,
		  type: 'bearer',
		};
		if (token) {
		  secret.value = { token };
		} else {
		  secret.usePrevious = true;
		}
		fs.writeFileSync(process.env.CLAUDE_CONFIGURE_OUTPUT, JSON.stringify({ files: [], secrets: [secret] }));
	NODE_EOF
}

PREVIOUS_ENV=$(previous_env)

ENV_NAME=""
TOKEN=""
KEEP_PREVIOUS=""
while [ -z "$ENV_NAME" ]; do
	echo "How should Claude Code authenticate?"
	echo "  1) Anthropic API key"
	echo "  2) Web login with a Claude subscription (claude setup-token)"
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
		TOKEN=$(collect_api_key) || {
			echo "An API key is required." >&2
			ENV_NAME=""
			continue
		}
		;;
	2)
		ENV_NAME="$OAUTH_ENV"
		TOKEN=$(collect_setup_token) || {
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
		KEEP_PREVIOUS=""
	fi
done

if [ -n "$KEEP_PREVIOUS" ]; then
	write_output "$ENV_NAME"
else
	write_output "$ENV_NAME" "$TOKEN"
fi

echo
echo "Claude Code configuration complete."
