#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# claude-code Definition). Launches Claude Code interactively, lets the user
# configure and log in to it however they normally would, then inspects what
# claude itself wrote to capture the result. Writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# Claude Code's own onboarding already offers the choice this script used to
# reimplement: sign in with a Claude subscription, or with an Anthropic
# Console account. Both write their result to disk, so this script does not
# need to drive either flow itself — it launches a bare `claude`, then reads
# whichever of these the user's session produced:
#   - A Claude subscription login (`claude /login`, chosen from inside the
#     session) writes the rotating OAuth blob (access token + refresh token +
#     expiry) to ~/.claude/.credentials.json. The whole blob, plus the fixed
#     Anthropic token endpoint and client id, is stored as an `oauth` secret
#     (CLAUDE_CODE_OAUTH_TOKEN) so the control plane can refresh the access
#     token as it expires.
#   - An Anthropic Console account login writes a long-lived managed key to
#     `primaryApiKey` in ~/.claude.json. That value is stored as a plain
#     bearer secret (ANTHROPIC_API_KEY).
# Whichever the user picked, it is verified with a non-interactive `claude -p`
# check before being accepted.
#
# It also captures ~/.claude/settings.json as-is and returns it as a harness
# file, so anything the user changes in the session (theme, model, statusline,
# ...) becomes the harness's default going forward. ~/.claude.json is never
# captured as a file: besides holding the credential this script already
# extracts on its own, it carries this sandbox's own per-workspace trust map
# (ensure_workspace_trusted, below), which is specific to the ephemeral
# configure sandbox and must not override a real sandbox's own trust state.
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each one's value is
# available as $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real
# credential on the way out, so the old credential can be exercised here
# without ever being readable in this sandbox. Keeping it is reported back as
# usePrevious, not as a value.
set -eu

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json
CREDENTIALS_FILE="$HOME/.claude/.credentials.json"
CLAUDE_CONFIG_FILE="$HOME/.claude.json"
SETTINGS_FILE="$HOME/.claude/settings.json"
SETTINGS_PATH=".claude/settings.json"
CREDENTIALS_PATH=".claude/.credentials.json"

# The expiry written into the delivered credentials file: far enough out that
# Claude Code never attempts a rotation. This is not a lie about the real token
# — what the file carries is a sentinel, which genuinely does not expire, and
# the control plane refreshes the credential behind it. 2100-01-01, in the
# milliseconds Claude Code stores.
CREDENTIALS_EXPIRES_AT=4102444800000

# The workspace the configure sandbox runs in. Unlike a run sandbox it has no
# source, so the image's .claude.json template trusts nothing — we mark the
# workspace trusted below so `claude` opens straight into onboarding/login
# rather than stopping at the trust dialog.
WORKSPACE_DIR="${DISCOBOX_WORKING_ROOT:-/workspace}"

API_KEY_ENV=ANTHROPIC_API_KEY
OAUTH_ENV=CLAUDE_CODE_OAUTH_TOKEN

# Fixed Claude Code OAuth parameters. These are properties of the Claude Code
# public OAuth client, not of any one login, so they are baked into the harness
# rather than read from the credentials file. The control plane uses them to
# refresh the access token via grant_type=refresh_token.
OAUTH_TOKEN_URL="https://console.anthropic.com/v1/oauth/token"
OAUTH_CLIENT_ID="9d1c250a-e61b-44d9-88ed-5944d1962f5e"

# Holds the collected OAuth payload JSON between extraction and write_output.
OAUTH_PAYLOAD_FILE=""

# Banner colors. The instructions below compete with a TUI that is about to
# repaint the screen, so the required steps are emphasized rather than left to
# be picked out of a wall of text. Every name is defined either way, so `set -u`
# holds and the wording never depends on whether color is on.
#
# NO_COLOR is honored the same way the CLI honors it. Both streams have to be a
# terminal, not just stdout: the banner goes to stdout and the failures to
# stderr, and either may be redirected into a log that nobody wants escape
# sequences in.
if [ -t 1 ] && [ -t 2 ] && [ -z "${NO_COLOR:-}" ] && [ -n "${TERM:-}" ] && [ "${TERM:-}" != dumb ]; then
	C_RESET=$(printf '\033[0m')
	C_BOLD=$(printf '\033[1m')
	C_WARN=$(printf '\033[33m')
	C_CMD=$(printf '\033[36m')
	C_ERR=$(printf '\033[31m')
else
	C_RESET=""
	C_BOLD=""
	C_WARN=""
	C_CMD=""
	C_ERR=""
fi

# ensure_workspace_trusted marks the workspace (and the current directory) as
# already trusted in ~/.claude.json, so the interactive `claude` launch below
# (and the `claude -p` verification) run without stopping at the trust dialog
# first. It merges into the file the image installed rather than replacing it,
# and touches only trust/onboarding, never a credential.
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

# extract_oauth_payload reads the rotating OAuth blob a subscription `/login`
# wrote to $CREDENTIALS_FILE, writes {token,refreshToken,tokenUrl,clientId[,
# accessTokenExpiresAt,scopes,subscriptionType]} to $OAUTH_PAYLOAD_FILE, and
# echoes just the access token so the caller can verify it like any bearer.
# Fails if no valid OAuth credential is present. $OAUTH_PAYLOAD_FILE must
# already be set by the caller.
#
# scopes and subscriptionType are not credentials — they describe what the login
# is allowed to do, and Claude Code gates features on the ones recorded next to
# the token (Remote Control needs `user:profile`). This is the only moment they
# can be known: they come back from the authorization server during /login and
# appear nowhere else. Copy them out here rather than assuming a set later —
# which scopes a login carries depends on the account and the flow, and claiming
# one the token lacks turns a clear client-side refusal into an upstream 401.
extract_oauth_payload() {
	[ -f "$CREDENTIALS_FILE" ] || return 1
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
		// Copied verbatim, never defaulted: an absent scope list has to stay
		// absent so nothing downstream can mistake a guess for what the
		// authorization server actually granted.
		if (Array.isArray(oauth.scopes)) {
			const scopes = oauth.scopes.filter((s) => typeof s === 'string' && s);
			if (scopes.length) {
				out.scopes = scopes;
			}
		}
		if (typeof oauth.subscriptionType === 'string' && oauth.subscriptionType) {
			out.subscriptionType = oauth.subscriptionType;
		}
		process.stdout.write(JSON.stringify(out));
	NODE_EOF
	) || return 1
	[ -n "$collected_payload" ] || return 1
	printf '%s' "$collected_payload" > "$OAUTH_PAYLOAD_FILE"
	CLAUDE_CONFIGURE_PAYLOAD="$collected_payload" node -e \
		'process.stdout.write(JSON.parse(process.env.CLAUDE_CONFIGURE_PAYLOAD).token)'
}

# extract_primary_api_key reads the long-lived managed key an "Anthropic
# Console account" login wrote to `primaryApiKey` in $CLAUDE_CONFIG_FILE, and
# echoes it. Fails if none is present.
extract_primary_api_key() {
	[ -f "$CLAUDE_CONFIG_FILE" ] || return 1
	CLAUDE_CONFIGURE_CONFIG_FILE="$CLAUDE_CONFIG_FILE" node <<-'NODE_EOF'
		const fs = require('fs');
		let config = {};
		try {
			config = JSON.parse(fs.readFileSync(process.env.CLAUDE_CONFIGURE_CONFIG_FILE, 'utf8')) || {};
		} catch (err) {
			config = {};
		}
		if (typeof config.primaryApiKey !== 'string' || !config.primaryApiKey) {
			process.exit(3);
		}
		process.stdout.write(config.primaryApiKey);
	NODE_EOF
}

# clear_captured_credential removes whatever claude wrote for a candidate that
# just failed verification, so a retry's detection cannot mistake a stale
# artifact from an earlier attempt in this same sandbox for a fresh one.
clear_captured_credential() {
	rm -f "$CREDENTIALS_FILE"
	if [ -f "$CLAUDE_CONFIG_FILE" ]; then
		CLAUDE_CONFIGURE_CONFIG_FILE="$CLAUDE_CONFIG_FILE" node <<-'NODE_EOF'
			const fs = require('fs');
			const file = process.env.CLAUDE_CONFIGURE_CONFIG_FILE;
			let config = null;
			try {
				config = JSON.parse(fs.readFileSync(file, 'utf8')) || {};
			} catch (err) {
				config = null;
			}
			if (config && 'primaryApiKey' in config) {
				delete config.primaryApiKey;
				fs.writeFileSync(file, JSON.stringify(config, null, 2));
			}
		NODE_EOF
	fi
}

# confirm_launch holds the instructions on screen until the user is ready.
# claude draws a TUI that repaints the terminal, so anything printed right
# before launching it is gone before it can be read -- and more so as Claude
# Code moves to a full-screen alternate-screen UI. Waiting for Enter means the
# banner is read while it is still the only thing on screen.
#
# End of input is not "yes": nobody is there, and launching an interactive TUI
# at nobody wedges the configure flow rather than failing it.
confirm_launch() {
	printf '%s' "${C_BOLD}Press Enter to start Claude Code, then run ${C_CMD}/login${C_RESET}${C_BOLD}.${C_RESET} "
	if ! read -r _launch_ack; then
		echo >&2
		echo "No input; aborting without configuring Claude Code." >&2
		exit 1
	fi
	echo
}

# confirm_retry asks whether to launch claude again after an attempt produced no
# usable credential. Every retry goes through here so the loop can only turn
# when a person asks it to: an attempt that fails without reaching the user --
# claude refusing to start, say -- fails again the moment it is retried, and
# looping on that is a busy loop, not a retry. End of input means nobody is
# there to answer, which fails the configure flow rather than spinning, the same
# rule the keep/replace prompt follows.
confirm_retry() {
	printf 'Try again? [Y/n] '
	if ! read -r retry_choice; then
		echo >&2
		echo "No input; aborting without configuring Claude Code." >&2
		exit 1
	fi
	echo
	case "${retry_choice:-y}" in
	[nN]*)
		echo "Aborting without configuring Claude Code." >&2
		exit 1
		;;
	esac
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

# write_output records the result: the credential as a secret, plus a
# snapshot of $SETTINGS_FILE as a harness file. Shapes:
#   - keep previous:  usePrevious marker, no value (KEEP_PREVIOUS set).
#   - oauth login:    the full rotating blob from $OAUTH_PAYLOAD_FILE, type oauth.
#   - api key:        a plain bearer { token }.
write_output() {
	write_env=$1
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	CLAUDE_CONFIGURE_ENV_NAME="$write_env" \
		CLAUDE_CONFIGURE_NAME="$(env_label "$write_env")" \
		CLAUDE_CONFIGURE_TYPE="${OUTPUT_TYPE:-bearer}" \
		CLAUDE_CONFIGURE_TOKEN="${OUTPUT_TOKEN:-}" \
		CLAUDE_CONFIGURE_KEEP_PREVIOUS="${KEEP_PREVIOUS:-}" \
		CLAUDE_CONFIGURE_PAYLOAD_FILE="${OAUTH_PAYLOAD_FILE:-}" \
		CLAUDE_CONFIGURE_SETTINGS_FILE="$SETTINGS_FILE" \
		CLAUDE_CONFIGURE_SETTINGS_PATH="$SETTINGS_PATH" \
		CLAUDE_CONFIGURE_CREDENTIALS_PATH="$CREDENTIALS_PATH" \
		CLAUDE_CONFIGURE_OAUTH_ENV="$OAUTH_ENV" \
		CLAUDE_CONFIGURE_CREDENTIALS_EXPIRES_AT="$CREDENTIALS_EXPIRES_AT" \
		CLAUDE_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		CLAUDE_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const secret = {
			envName: process.env.CLAUDE_CONFIGURE_ENV_NAME,
			name: process.env.CLAUDE_CONFIGURE_NAME,
			type: process.env.CLAUDE_CONFIGURE_TYPE || 'bearer',
		};
		let payload = null;
		if (process.env.CLAUDE_CONFIGURE_KEEP_PREVIOUS) {
			secret.usePrevious = true;
		} else if (secret.type === 'oauth') {
			payload = JSON.parse(fs.readFileSync(process.env.CLAUDE_CONFIGURE_PAYLOAD_FILE, 'utf8'));
			secret.value = payload;
		} else {
			secret.value = { token: process.env.CLAUDE_CONFIGURE_TOKEN };
		}
		const files = [];
		try {
			const content = fs.readFileSync(process.env.CLAUDE_CONFIGURE_SETTINGS_FILE, 'utf8');
			files.push({ path: process.env.CLAUDE_CONFIGURE_SETTINGS_PATH, content });
		} catch (err) {
			// No settings file to capture (unexpected — the image bakes one — but
			// not a reason to fail the whole configure flow over it).
		}

		// The subscription credential is delivered as a file rather than an env
		// var. Claude Code decides what a credential may do from the `scopes`
		// recorded beside it, and an env-var token carries none — which is what
		// limits it to inference and refuses Remote Control. The scopes copied
		// out of the real login (extract_oauth_payload) are replayed here.
		//
		// What lands in the file is the **sentinel**, not the credential: the
		// content is a template, and each sandbox renders its own sentinel into
		// it. The proxy swaps it outbound exactly as it did for the env var, so
		// no credential is written to a file anywhere.
		//
		// expiresAt is deliberately far future. Claude Code must never try to
		// rotate this: the refresh token stays in the control plane, so a refresh
		// from inside a sandbox could not succeed. Nor does it need to — a
		// sentinel does not expire, and the control plane keeps the real token
		// fresh behind it. refreshToken is a placeholder for the same reason:
		// the field must be present, and it is never reachable.
		const credentialsFile = (oauth) => ({
			path: process.env.CLAUDE_CONFIGURE_CREDENTIALS_PATH,
			template: true,
			content: JSON.stringify({
				claudeAiOauth: {
					// Dotted field access, never a quoted map key. This whole
				// object is JSON-encoded to become the file's content, so a
				// quote inside a template action comes out backslash-escaped
				// and Go's parser rejects it ("unexpected \\ in operand") —
				// which breaks every sandbox launch while configure still
				// reports success. A secret's env name is always a valid
				// template field name (HarnessConfigEnvVarNamePattern), so
				// there is nothing to quote.
				accessToken: `{{ .secrets.${process.env.CLAUDE_CONFIGURE_OAUTH_ENV} }}`,
					refreshToken: 'discobox-refresh-happens-in-the-control-plane',
					expiresAt: Number(process.env.CLAUDE_CONFIGURE_CREDENTIALS_EXPIRES_AT),
					scopes: oauth.scopes,
					subscriptionType: oauth.subscriptionType,
				},
			}, null, 2),
		});

		if (payload && Array.isArray(payload.scopes) && payload.scopes.length) {
			files.push(credentialsFile(payload));
		} else if (secret.usePrevious) {
			// Keeping the previous credential keeps what was captured with it:
			// the scopes live in the file this flow wrote last time, and the seed
			// hands it back. Re-emitting it verbatim is what keeps it, since the
			// output replaces the configured file set wholesale.
			try {
				const previous = JSON.parse(fs.readFileSync(process.env.CLAUDE_CONFIGURE_PREVIOUS, 'utf8')) || {};
				const kept = (previous.files || []).find((f) => f && f.path === process.env.CLAUDE_CONFIGURE_CREDENTIALS_PATH);
				if (kept) {
					files.push(kept);
				}
			} catch (err) {
				// No previous file to replay; the credential still works, it is
				// just limited to inference until the next full reconfigure.
			}
		}
		fs.writeFileSync(process.env.CLAUDE_CONFIGURE_OUTPUT, JSON.stringify({ files, secrets: [secret] }));
	NODE_EOF
}

PREVIOUS_ENV=$(previous_env)

# Trust the workspace up front so every claude invocation below — the
# interactive session and the `claude -p` verification — runs without a trust
# prompt.
ensure_workspace_trusted

ENV_NAME=""
TOKEN=""
OUTPUT_TYPE="bearer"
KEEP_PREVIOUS=""

if [ -n "$PREVIOUS_ENV" ]; then
	printf 'Keep the existing credential (%s)? [Y/n] ' "$(env_label "$PREVIOUS_ENV")"
	# End of input means nobody is there to answer: stop rather than spin.
	if ! read -r keep_choice; then
		echo >&2
		echo "No input; aborting without configuring Claude Code." >&2
		exit 1
	fi
	case "${keep_choice:-y}" in
	[nN]*) ;;
	*)
		ENV_NAME="$PREVIOUS_ENV"
		if [ "$ENV_NAME" = "$OAUTH_ENV" ]; then
			OUTPUT_TYPE="oauth"
		else
			OUTPUT_TYPE="bearer"
		fi
		KEEP_PREVIOUS=yes
		eval "TOKEN=\${PREV_$PREVIOUS_ENV}"
		echo "Checking the existing credential…"
		if ! verify_credential "$ENV_NAME" "$TOKEN"; then
			echo
			echo "The existing credential no longer works; let's set up a new one." >&2
			ENV_NAME=""
			TOKEN=""
			OUTPUT_TYPE="bearer"
			KEEP_PREVIOUS=""
		fi
		;;
	esac
	echo
fi

while [ -z "$ENV_NAME" ]; do
	printf '%s\n' "${C_BOLD}${C_WARN}================================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up Claude Code — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}================================================================${C_RESET}"
	echo
	echo "Discobox is about to start Claude Code so you can sign in and set"
	echo "it up. This is a throwaway setup sandbox: it exists only to capture"
	echo "your login and settings, and it is deleted the moment you leave."
	printf '%s\n' "${C_BOLD}Do not start real work in here — none of it is kept.${C_RESET}"
	echo
	printf '%s\n' "${C_BOLD}You must do both of these:${C_RESET}"
	echo
	printf '%s\n' "  1. ${C_BOLD}${C_CMD}/login${C_RESET}   Sign in, with a Claude subscription or an Anthropic"
	echo "              Console account. Nothing can be saved without it."
	printf '%s\n' "  2. ${C_BOLD}${C_CMD}/exit${C_RESET}    Leave Claude Code when you're done. Setup only"
	echo "              finishes once you exit — staying in blocks it."
	echo
	echo "Worth doing while you're in there:"
	echo
	printf '%s\n' "  ${C_CMD}/model${C_RESET}      Pick the model this harness runs with"
	printf '%s\n' "  ${C_CMD}/config${C_RESET}     Theme, statusline, and the rest"
	echo
	printf '%s\n' "Then ${C_BOLD}${C_CMD}/exit${C_RESET} (or Ctrl-D) and Discobox will save your setup."
	echo
	confirm_launch

	# Start from no credential at all. This sandbox is handed the same configured
	# files a run sandbox gets, which now include a templated credentials file —
	# and rendered here it names a sentinel this sandbox does not have, since the
	# configure flow binds PREV_-prefixed names instead. Left in place it would
	# read as "already signed in" and suppress the login this whole flow exists
	# to perform.
	rm -f "$CREDENTIALS_FILE"

	# claude needs a real TTY for its interactive UI; run it under script.
	set +e
	script -q -e -c 'claude' /dev/null
	claude_status=$?
	set -e
	echo

	OAUTH_PAYLOAD_FILE=$(mktemp)
	if TOKEN=$(extract_oauth_payload); then
		ENV_NAME="$OAUTH_ENV"
		OUTPUT_TYPE="oauth"
	elif TOKEN=$(extract_primary_api_key); then
		ENV_NAME="$API_KEY_ENV"
		OUTPUT_TYPE="bearer"
		rm -f "$OAUTH_PAYLOAD_FILE"
		OAUTH_PAYLOAD_FILE=""
	else
		rm -f "$OAUTH_PAYLOAD_FILE"
		OAUTH_PAYLOAD_FILE=""
		if [ "$claude_status" -ne 0 ]; then
			# Not "the user skipped the login": claude refused to run at all, and
			# saying so points at the message it printed just above rather than
			# asking again as if a step had been missed.
			printf '%s\n' "${C_ERR}${C_BOLD}Claude Code exited with status $claude_status without configuring a credential.${C_RESET}" >&2
		else
			printf '%s\n' "${C_ERR}${C_BOLD}You left Claude Code without signing in, so there is nothing" >&2
			printf '%s\n' "to save. Setup needs you to run /login inside Claude Code" >&2
			printf '%s\n' "before you /exit.${C_RESET}" >&2
		fi
		confirm_retry
		continue
	fi

	echo "Verifying the credential…"
	if ! verify_credential "$ENV_NAME" "$TOKEN"; then
		echo
		clear_captured_credential
		[ -n "$OAUTH_PAYLOAD_FILE" ] && rm -f "$OAUTH_PAYLOAD_FILE"
		ENV_NAME=""
		TOKEN=""
		OUTPUT_TYPE="bearer"
		OAUTH_PAYLOAD_FILE=""
		confirm_retry
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
