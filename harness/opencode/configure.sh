#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# opencode Definition). Collects one or both provider API keys, verifies each one
# actually works with `opencode run`, and writes them to
# /run/discobox/harness-configure.json for discobox to apply to the HarnessConfig.
#
# Reconfigure: /run/discobox/harness-previous-config.json lists the secrets a
# previous run stored, without their values. Each one's value is available as
# $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real credential on the
# way out, so an old key can be exercised here without ever being readable in
# this sandbox. Keeping one is reported back as usePrevious, not as a value.
#
# Output is authoritative and replaces the previous configuration wholesale, so
# a provider left out here is removed from the harness config.
#
# Only credentials are captured; this flow returns no files.
set -eu

PREVIOUS_CONFIG=/run/discobox/harness-previous-config.json
OUTPUT=/run/discobox/harness-configure.json

ANTHROPIC_ENV=ANTHROPIC_API_KEY
OPENAI_ENV=OPENAI_API_KEY

provider_of() {
	if [ "$1" = "$ANTHROPIC_ENV" ]; then echo anthropic; else echo openai; fi
}

label_of() {
	if [ "$1" = "$ANTHROPIC_ENV" ]; then echo 'Anthropic API key'; else echo 'OpenAI API key'; fi
}

other_env() {
	if [ "$1" = "$ANTHROPIC_ENV" ]; then echo "$OPENAI_ENV"; else echo "$ANTHROPIC_ENV"; fi
}

previous_value_of() {
	eval "echo \"\${PREV_$1:-}\""
}

# has_previous reports whether a previous configure run stored a key for $1 *and*
# its PREV_ value is actually set. A seeded secret with no value behind it cannot
# be reused.
has_previous() {
	[ -f "$PREVIOUS_CONFIG" ] || return 1
	[ -n "$(previous_value_of "$1")" ] || return 1
	OPENCODE_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" OPENCODE_CONFIGURE_ENV="$1" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.OPENCODE_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const known = (previous.secrets || []).some((s) => s && s.envName === process.env.OPENCODE_CONFIGURE_ENV);
		process.exit(known ? 0 : 1);
	NODE_EOF
}

# verify_credential checks one provider's key with only that key in the
# environment, so the check exercises exactly what a real sandbox will run with
# and cannot pass on the strength of the other provider's credential.
#
# The model is discovered rather than hardcoded: `opencode models` lists only the
# providers whose credential is present, so asking with just this key set both
# picks a model that exists in this image's opencode and proves the key is wired
# to the provider we think it is.
verify_credential() {
	verify_env=$1
	verify_token=$2
	verify_provider=$(provider_of "$verify_env")
	verify_other=$(other_env "$verify_env")

	verify_model=$(env -u "$verify_other" "$verify_env=$verify_token" \
		opencode models 2>/dev/null | grep "^$verify_provider/" | head -n 1 || true)
	if [ -z "$verify_model" ]; then
		echo
		echo "opencode does not offer any $verify_provider model for that key." >&2
		return 1
	fi

	set +e
	verify_output=$(env -u "$verify_other" "$verify_env=$verify_token" \
		timeout 180 opencode run --model "$verify_model" 'Reply with exactly: discobox-ok' 2>&1)
	verify_status=$?
	set -e

	if [ "$verify_status" -ne 0 ] || [ -z "$verify_output" ]; then
		echo
		echo "The $(label_of "$verify_env") did not work:" >&2
		echo "$verify_output" >&2
		return 1
	fi
	echo "opencode replied with $verify_model: $verify_output"
	return 0
}

# collect_api_key reads a key without echoing it. It returns 1 when input has
# ended (nobody is there to answer) and 2 when the answer was empty, which for
# opencode means "skip this provider" rather than an error.
collect_api_key() {
	printf 'Enter an %s, or press Enter to skip: ' "$(label_of "$1")" >&2
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

# configure_provider settles one provider, setting RESULT_TOKEN (empty when the
# provider is left out) and RESULT_KEEP (set when the existing secret is kept).
configure_provider() {
	target_env=$1
	RESULT_TOKEN=""
	RESULT_KEEP=""

	while :; do
		if has_previous "$target_env"; then
			echo "opencode already has a configured $(label_of "$target_env")."
			echo "  1) Keep the existing key"
			echo "  2) Enter a new key"
			echo "  3) Remove it"
			printf 'Choose [1]: '
			# End of input means nobody is there to answer: stop rather than spin.
			if ! read -r choice; then
				echo >&2
				echo "No input; aborting without configuring opencode." >&2
				exit 1
			fi
			echo
			case "${choice:-1}" in
			1)
				# The sentinel, not the credential: good enough to verify with, and
				# worthless if it leaks.
				RESULT_TOKEN=$(previous_value_of "$target_env")
				RESULT_KEEP=yes
				;;
			2) RESULT_TOKEN="" ;;
			3)
				RESULT_TOKEN=""
				RESULT_KEEP=""
				return 0
				;;
			*)
				echo "Choose 1, 2, or 3." >&2
				continue
				;;
			esac
		fi

		if [ -z "$RESULT_KEEP" ]; then
			collect_status=0
			RESULT_TOKEN=$(collect_api_key "$target_env") || collect_status=$?
			if [ "$collect_status" -eq 1 ]; then
				echo "No input; aborting without configuring opencode." >&2
				exit 1
			fi
			if [ "$collect_status" -ne 0 ]; then
				# Skipped: leave this provider out.
				RESULT_TOKEN=""
				return 0
			fi
		fi

		echo "Checking the $(label_of "$target_env") with a test prompt…"
		if verify_credential "$target_env" "$RESULT_TOKEN"; then
			return 0
		fi
		echo
		echo "Let's try again."
		echo
		RESULT_TOKEN=""
		RESULT_KEEP=""
	done
}

configure_provider "$ANTHROPIC_ENV"
ANTHROPIC_TOKEN="$RESULT_TOKEN"
ANTHROPIC_KEEP="$RESULT_KEEP"
echo

configure_provider "$OPENAI_ENV"
OPENAI_TOKEN="$RESULT_TOKEN"
OPENAI_KEEP="$RESULT_KEEP"
echo

if [ -z "$ANTHROPIC_TOKEN" ] && [ -z "$OPENAI_TOKEN" ]; then
	echo "At least one provider API key is required." >&2
	exit 1
fi

mkdir -p /run/discobox
OPENCODE_CONFIGURE_ANTHROPIC_TOKEN="$ANTHROPIC_TOKEN" \
	OPENCODE_CONFIGURE_ANTHROPIC_KEEP="$ANTHROPIC_KEEP" \
	OPENCODE_CONFIGURE_OPENAI_TOKEN="$OPENAI_TOKEN" \
	OPENCODE_CONFIGURE_OPENAI_KEEP="$OPENAI_KEEP" \
	OPENCODE_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
	const fs = require('fs');
	const secrets = [];
	const add = (envName, name, token, keep) => {
	  if (keep) {
	    secrets.push({ envName, name, type: 'bearer', usePrevious: true });
	  } else if (token) {
	    secrets.push({ envName, name, type: 'bearer', value: { token } });
	  }
	};
	add('ANTHROPIC_API_KEY', 'Anthropic API key',
	  process.env.OPENCODE_CONFIGURE_ANTHROPIC_TOKEN, process.env.OPENCODE_CONFIGURE_ANTHROPIC_KEEP);
	add('OPENAI_API_KEY', 'OpenAI API key',
	  process.env.OPENCODE_CONFIGURE_OPENAI_TOKEN, process.env.OPENCODE_CONFIGURE_OPENAI_KEEP);
	fs.writeFileSync(process.env.OPENCODE_CONFIGURE_OUTPUT, JSON.stringify({ files: [], secrets }));
NODE_EOF

echo "opencode configuration complete."
