#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# codex-cli Definition). Collects an OpenAI API key, verifies it actually works
# with `codex exec`, and writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each value is available as
# $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real credential on the
# way out, so the old key can be exercised here without ever being readable in
# this sandbox. Keeping it is reported back as usePrevious, not as a value.
#
# Only the credential is captured; this flow returns no files.
set -eu

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json

API_KEY_ENV=OPENAI_API_KEY
SECRET_LABEL='OpenAI API key'

# has_previous reports whether a previous configure run stored a key for
# $API_KEY_ENV *and* its PREV_ value is actually set. A seeded secret with no
# value behind it cannot be reused.
has_previous() {
	[ -f "$PREVIOUS_CONFIG" ] || return 1
	[ -n "${PREV_OPENAI_API_KEY:-}" ] || return 1
	CODEX_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" CODEX_CONFIGURE_ENV="$API_KEY_ENV" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const known = (previous.secrets || []).some((s) => s && s.envName === process.env.CODEX_CONFIGURE_ENV);
		process.exit(known ? 0 : 1);
	NODE_EOF
}

# verify_credential runs a trivial non-interactive prompt with the chosen key in
# the environment, so the check exercises exactly what a real sandbox will run
# with. Nothing in this flow performs a ChatGPT login, so there is no auth file
# to move aside — unlike the claude-code flow.
verify_credential() {
	verify_token=$1
	verify_message=$(mktemp)
	verify_log=$(mktemp)

	set +e
	env "$API_KEY_ENV=$verify_token" timeout 180 codex exec 'Reply with exactly: discobox-ok' \
		--skip-git-repo-check \
		--dangerously-bypass-approvals-and-sandbox \
		--output-last-message "$verify_message" >"$verify_log" 2>&1
	verify_status=$?
	set -e

	verify_reply=$(cat "$verify_message" 2>/dev/null || true)
	if [ "$verify_status" -ne 0 ] || [ -z "$verify_reply" ]; then
		echo
		echo "The $SECRET_LABEL did not work:" >&2
		# codex retries an unauthorized request several times; the tail carries the
		# actual reason without replaying the whole storm.
		tail -n 5 "$verify_log" >&2
		rm -f "$verify_message" "$verify_log"
		return 1
	fi
	rm -f "$verify_message" "$verify_log"
	echo "Codex replied: $verify_reply"
	return 0
}

# collect_api_key reads a key without echoing it. It returns 1 when input has
# ended (nobody is there to answer) and 2 when the answer was empty, so the
# caller can abort in the first case and re-prompt in the second.
collect_api_key() {
	printf 'Enter your OpenAI API key: ' >&2
	stty -echo 2>/dev/null || true
	if ! read -r collected_token; then
		stty echo 2>/dev/null || true
		echo >&2
		return 1
	fi
	stty echo 2>/dev/null || true
	echo >&2
	[ -n "$collected_token" ] || return 2
	echo "$collected_token"
}

# write_output records the result: either the key just collected, or a
# usePrevious marker that keeps the secret the control plane already holds.
write_output() {
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	CODEX_CONFIGURE_ENV_NAME="$API_KEY_ENV" \
		CODEX_CONFIGURE_NAME="$SECRET_LABEL" \
		CODEX_CONFIGURE_TOKEN="${1:-}" \
		CODEX_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const token = process.env.CODEX_CONFIGURE_TOKEN;
		const secret = {
		  envName: process.env.CODEX_CONFIGURE_ENV_NAME,
		  name: process.env.CODEX_CONFIGURE_NAME,
		  type: 'bearer',
		};
		if (token) {
		  secret.value = { token };
		} else {
		  secret.usePrevious = true;
		}
		fs.writeFileSync(process.env.CODEX_CONFIGURE_OUTPUT, JSON.stringify({ files: [], secrets: [secret] }));
	NODE_EOF
}

PREVIOUS_AVAILABLE=""
if has_previous; then
	PREVIOUS_AVAILABLE=yes
fi

TOKEN=""
KEEP_PREVIOUS=""
while :; do
	if [ -n "$PREVIOUS_AVAILABLE" ]; then
		echo "Codex already has a configured $SECRET_LABEL."
		echo "  1) Keep the existing key"
		echo "  2) Enter a new key"
		printf 'Choose [1]: '
		# End of input means nobody is there to answer: stop rather than spin.
		if ! read -r choice; then
			echo >&2
			echo "No input; aborting without configuring Codex." >&2
			exit 1
		fi
		echo
		case "${choice:-1}" in
		1) KEEP_PREVIOUS=yes ;;
		2) KEEP_PREVIOUS="" ;;
		*)
			echo "Choose 1 or 2." >&2
			continue
			;;
		esac
	fi

	if [ -n "$KEEP_PREVIOUS" ]; then
		# The sentinel, not the credential: good enough to verify with, and
		# worthless if it leaks.
		TOKEN="$PREV_OPENAI_API_KEY"
	else
		collect_status=0
		TOKEN=$(collect_api_key) || collect_status=$?
		if [ "$collect_status" -eq 1 ]; then
			echo "No input; aborting without configuring Codex." >&2
			exit 1
		fi
		if [ "$collect_status" -ne 0 ]; then
			echo "An $SECRET_LABEL is required." >&2
			[ -n "$PREVIOUS_AVAILABLE" ] || exit 1
			continue
		fi
	fi

	echo "Checking the key with a test prompt…"
	if verify_credential "$TOKEN"; then
		break
	fi
	echo
	echo "Let's try again."
	echo
	TOKEN=""
	KEEP_PREVIOUS=""
done

if [ -n "$KEEP_PREVIOUS" ]; then
	write_output
else
	write_output "$TOKEN"
fi

echo
echo "Codex configuration complete."
