#!/bin/sh
# Runs as the configure sandbox's primary terminal (see "Configure flows"
# in harness/DESIGN.md). Launches Codex interactively, lets the user sign in and
# configure it however they normally would, then inspects what codex itself
# wrote to capture the result. Writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# Codex's own onboarding already offers every sign-in this script would
# otherwise reimplement — ChatGPT in a browser (its localhost:1455 callback is
# forwarded in from the user's machine; see config.ports in image.json), ChatGPT
# by device code (the fallback when that port could not be bound there), or an
# API key — and each writes its result to $CODEX_HOME/auth.json. So this script launches a bare
# `codex` and then reads whichever shape that file came back in:
#   - A ChatGPT sign-in writes {"tokens": {id_token, access_token,
#     refresh_token, account_id}, "last_refresh": ...}. The access token is
#     stored as an `oauth` secret (CODEX_OAUTH_TOKEN) together with the refresh
#     token and OpenAI's fixed token endpoint and client id, so the control
#     plane can refresh it as it expires.
#   - An API key sign-in writes {"OPENAI_API_KEY": "sk-..."} — a long-lived key,
#     stored as a plain `token` secret (OPENAI_API_KEY).
# Whichever the user picked, it is verified with a non-interactive `codex exec`
# before being accepted.
#
# Neither credential is exported as an environment variable: both are declared
# `delivery: file`, and the sentinel is rendered into the ~/.codex/auth.json
# this flow returns. That is not tidiness. The interactive TUI does not read
# CODEX_API_KEY at all (only `codex exec` does), and no environment variable
# carries a ChatGPT token, so auth.json is the one delivery both halves of the
# harness agree on.
#
# It also returns ~/.codex/config.toml as-is (minus the [projects] trust map,
# see write_output), so anything the user changes in the session — model,
# theme, statusline — becomes the harness's default going forward.
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each one's value is
# available as $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real
# credential on the way out, so the old credential can be exercised here
# without ever being readable in this sandbox. Keeping it is reported back as
# usePrevious, not as a value.
set -eu
HARNESS_CAPTURE_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
export HARNESS_CAPTURE_DIR

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json

# CODEX_HOME is codex's own override; default to the same ~/.codex the harness
# files are delivered to.
CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
AUTH_FILE="$CODEX_HOME_DIR/auth.json"
CONFIG_FILE="$CODEX_HOME_DIR/config.toml"
AUTH_PATH=".codex/auth.json"
CONFIG_PATH=".codex/config.toml"

API_KEY_ENV=OPENAI_API_KEY
OAUTH_ENV=CODEX_OAUTH_TOKEN

# Fixed OpenAI OAuth parameters. These are properties of the Codex CLI's public
# OAuth client, not of any one login, so they are baked into the harness rather
# than read from auth.json. The control plane uses them to refresh the access
# token via grant_type=refresh_token.

# The last_refresh written into the delivered auth.json: far enough out that
# codex never decides the credential is stale and rotates it itself. This is not
# a lie about the real token — what the file carries is a sentinel, which
# genuinely does not go stale, and the control plane refreshes the credential
# behind it. Codex rotates when last_refresh is older than 28 days, so a future
# date simply never triggers.
AUTH_LAST_REFRESH="2100-01-01T00:00:00Z"



# Holds the collected OAuth capture JSON (secret value plus the non-secret
# account claims the delivered auth.json needs) between detection and
# write_output.
OAUTH_CAPTURE_FILE=""

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

# env_label prints the human name recorded alongside the secret.
env_label() {
	if [ "$1" = "$API_KEY_ENV" ]; then
		echo "OpenAI API key"
	else
		echo "Codex ChatGPT sign-in"
	fi
}

# previous_env prints the env name of the credential a previous configure run
# stored, or nothing. Only names whose PREV_ variable is actually set count: a
# seeded secret with no value behind it cannot be reused.
previous_env() {
	[ -f "$PREVIOUS_CONFIG" ] || return 0
	CODEX_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const secrets = previous.secrets || [];
		for (const envName of ['CODEX_OAUTH_TOKEN', 'OPENAI_API_KEY']) {
			const known = secrets.some((s) => s && s.envName === envName);
			if (known && process.env['PREV_' + envName]) {
				process.stdout.write(envName + '\n');
				break;
			}
		}
	NODE_EOF
}

# Trust is a process override; portable user settings remain literal.
ensure_workspace_trusted() {
    trust_key=$(node -e 'process.stdout.write(JSON.stringify(process.cwd()))')
    export DISCOBOX_CODEX_TRUST="projects.$trust_key.trust_level=\"trusted\""
}

# extract_api_key reads the long-lived key an API key sign-in wrote to
# OPENAI_API_KEY in $AUTH_FILE, and echoes it. Fails if none is present.
extract_api_key() {
	[ -f "$AUTH_FILE" ] || return 1
	CODEX_CONFIGURE_AUTH_FILE="$AUTH_FILE" node <<-'NODE_EOF'
		const fs = require('fs');
		let auth = {};
		try {
			auth = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_AUTH_FILE, 'utf8')) || {};
		} catch (err) {
			auth = {};
		}
		const key = auth.OPENAI_API_KEY;
		if (typeof key !== 'string' || !key) {
			process.exit(3);
		}
		process.stdout.write(key);
	NODE_EOF
}

# extract_oauth_capture reads the rotating token set a ChatGPT sign-in wrote to
# $AUTH_FILE, writes {secret,tokens} to $OAUTH_CAPTURE_FILE, and echoes just the
# access token so the caller can compare it like any other value. Fails if no
# usable ChatGPT credential is present. $OAUTH_CAPTURE_FILE must already be set
# by the caller.
#
# `secret` is the credential: the access token plus the refresh material the
# control plane rotates it with. `tokens` is not — it is the account the login
# belongs to (id, plan, email), which codex needs beside the token to address
# the ChatGPT backend at all, and which is only knowable here. It is copied into
# the delivered auth.json as claims rather than as the signed id_token codex
# wrote (see write_output): the claims are what codex reads, and a signed
# identity assertion has no business sitting in a non-secret harness file.
extract_oauth_capture() {
	[ -f "$AUTH_FILE" ] || return 1
	CODEX_CONFIGURE_AUTH_FILE="$AUTH_FILE" \
		CODEX_CONFIGURE_CAPTURE_FILE="$OAUTH_CAPTURE_FILE" node <<-'NODE_EOF'
		const fs = require('fs');
		const path = require('path');
		const dir = process.env.HARNESS_CAPTURE_DIR;
		const { oauth } = require(path.join(dir, 'codex-normalize.cjs'));
		const definition = require(path.join(dir, 'codex-image.json')).harness.saveBack;
		let auth;
		try { auth = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_AUTH_FILE, 'utf8')); } catch { process.exit(3); }
		let result;
		try { result = oauth(auth, definition.providers.find(p => p.id === 'openai')); } catch { process.exit(3); }
		fs.writeFileSync(process.env.CODEX_CONFIGURE_CAPTURE_FILE, JSON.stringify(result));
		process.stdout.write(result.secret.token);
	NODE_EOF
}

# clear_captured_credential removes what codex wrote for a candidate that just
# failed verification, so a retry's detection cannot mistake a stale artifact
# from an earlier attempt in this same sandbox for a fresh one. codex writes
# auth.json whole on every sign-in, so removing it is the whole job.
clear_captured_credential() {
	rm -f "$AUTH_FILE"
}

# seed_previous_credential puts the existing credential back where codex reads
# it, so the session below opens already signed in.
#
# This is what lets a reconfigure be about anything else. Reconfigure is usually
# about settings -- model, theme, statusline -- and a flow that either kept the
# credential without launching codex, or launched it signed out, made changing a
# setting cost a fresh sign-in.
#
# What is written is the PREV_ sentinel, not the credential: the proxy swaps it
# on outbound requests, so the session is genuinely signed in while this sandbox
# never holds the real token. The sentinel is also how the change check works
# afterwards -- it is a value we know, so finding it still in place means nothing
# re-authenticated (see detect_credential).
#
# The seed is the auth.json the last run returned, replayed with the sentinel in
# place of its secret reference, so it carries the same account claims the real
# login had. A configuration made before this flow returned that file has
# nothing to replay and simply opens signed out, which is the one case where a
# sign-in is unavoidable.
#
# Whatever is not seeded is removed. A configured harness delivers its auth.json
# into this sandbox like any other, but its references resolve against secrets
# this sandbox does not have (they arrive PREV_-prefixed), so what lands is a
# credential-shaped file with nothing behind it — and codex reads that as
# "signed in", skipping the sign-in screen the user came here for.
seed_previous_credential() {
	seeded=""
	[ -n "$PREVIOUS_ENV" ] || { clear_captured_credential; return 0; }
	eval "SEEDED_SENTINEL=\${PREV_$PREVIOUS_ENV:-}"
	[ -n "$SEEDED_SENTINEL" ] || { clear_captured_credential; return 0; }
	seeded=$(CODEX_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		CODEX_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		CODEX_CONFIGURE_AUTH_FILE="$AUTH_FILE" \
		CODEX_CONFIGURE_SENTINEL="$SEEDED_SENTINEL" \
		CODEX_CONFIGURE_ENV_NAME="$PREVIOUS_ENV" node <<-'NODE_EOF'
		const fs = require('fs');
		const nodePath = require('path');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const kept = (previous.files || []).find((f) => f && f.path === process.env.CODEX_CONFIGURE_AUTH_PATH);
		if (!kept || typeof kept.content !== 'string') {
			process.stdout.write('');
			process.exit(0);
		}
        let content;
        try {
          const { resolveReferences } = require(nodePath.join(process.env.HARNESS_CAPTURE_DIR, 'harness-capture/runtime.cjs'));
          content = JSON.stringify(resolveReferences(JSON.parse(kept.content), {
            [process.env.CODEX_CONFIGURE_ENV_NAME]: process.env.CODEX_CONFIGURE_SENTINEL,
          }));
		} catch (err) {
			process.stdout.write('');
			process.exit(0);
		}
		const file = process.env.CODEX_CONFIGURE_AUTH_FILE;
		fs.mkdirSync(nodePath.dirname(file), { recursive: true });
		fs.writeFileSync(file, content, { mode: 0o600 });
		process.stdout.write('seeded');
	NODE_EOF
	)
	# Nothing to replay: the session opens signed out, and the sentinel must stop
	# being the yardstick for "unchanged" or detection would report a credential
	# nothing put there.
	if [ -z "$seeded" ]; then
		SEEDED_SENTINEL=""
		clear_captured_credential
	fi
}

# detect_credential works out what the session left behind, setting ENV_NAME,
# TOKEN, OUTPUT_TYPE and KEEP_PREVIOUS. Returns non-zero when there is nothing.
#
# A value equal to $SEEDED_SENTINEL was not written by a sign-in -- it is what
# seed_previous_credential put there -- so the credential is unchanged and is
# reported back as usePrevious rather than as a value. Anything else is a real
# sign-in and is captured.
#
# A *changed* credential wins over an unchanged one, in either shape. codex
# rewrites auth.json whole, so in practice only one shape is ever present; the
# rule still holds where it is not, and costs nothing.
detect_credential() {
	detected_key=""
	detected_oauth=""
	OAUTH_CAPTURE_FILE=$(mktemp)
	if detected_oauth=$(extract_oauth_capture); then :; else detected_oauth=""; fi
	if detected_key=$(extract_api_key); then :; else detected_key=""; fi

	if [ -n "$detected_oauth" ] && [ "$detected_oauth" != "$SEEDED_SENTINEL" ]; then
		ENV_NAME="$OAUTH_ENV"
		OUTPUT_TYPE="oauth"
		TOKEN="$detected_oauth"
		return 0
	fi
	if [ -n "$detected_key" ] && [ "$detected_key" != "$SEEDED_SENTINEL" ]; then
		ENV_NAME="$API_KEY_ENV"
		OUTPUT_TYPE="token"
		TOKEN="$detected_key"
		rm -f "$OAUTH_CAPTURE_FILE"
		OAUTH_CAPTURE_FILE=""
		return 0
	fi
	if [ -n "$SEEDED_SENTINEL" ] && { [ -n "$detected_oauth" ] || [ -n "$detected_key" ]; }; then
		ENV_NAME="$PREVIOUS_ENV"
		if [ "$ENV_NAME" = "$OAUTH_ENV" ]; then
			OUTPUT_TYPE="oauth"
		else
			OUTPUT_TYPE="token"
		fi
		TOKEN="$SEEDED_SENTINEL"
		KEEP_PREVIOUS=yes
		rm -f "$OAUTH_CAPTURE_FILE"
		OAUTH_CAPTURE_FILE=""
		return 0
	fi
	rm -f "$OAUTH_CAPTURE_FILE"
	OAUTH_CAPTURE_FILE=""
	return 1
}

# confirm_launch holds the instructions on screen until the user is ready.
# codex draws a full-screen TUI, so anything printed right before launching it
# is gone before it can be read. Waiting for Enter means the banner is read
# while it is still the only thing on screen.
#
# End of input is not "yes": nobody is there, and launching an interactive TUI
# at nobody wedges the configure flow rather than failing it.
confirm_launch() {
	if [ -n "$SEEDED_SENTINEL" ]; then
		printf '%s' "${C_BOLD}Press Enter to start Codex.${C_RESET} "
	else
		printf '%s' "${C_BOLD}Press Enter to start Codex, then sign in.${C_RESET} "
	fi
	if ! read -r _launch_ack; then
		echo >&2
		echo "No input; aborting without configuring Codex." >&2
		exit 1
	fi
	echo
}

# confirm_retry asks whether to launch codex again after an attempt produced no
# usable credential. Every retry goes through here so the loop can only turn
# when a person asks it to: an attempt that fails without reaching the user --
# codex refusing to start, say -- fails again the moment it is retried, and
# looping on that is a busy loop, not a retry. End of input means nobody is
# there to answer, which fails the configure flow rather than spinning.
confirm_retry() {
	printf 'Start Codex again? [Y/n] '
	if ! read -r retry_choice; then
		echo >&2
		echo "No input; aborting without configuring Codex." >&2
		exit 1
	fi
	echo
	case "${retry_choice:-y}" in
	[nN]*)
		echo "Aborting without configuring Codex." >&2
		exit 1
		;;
	esac
}

# verify_credential runs a trivial non-interactive prompt against the auth.json
# the session left behind, which is exactly what a real sandbox will run with.
# There is no environment variable to point codex at one credential instead of
# another -- the TUI reads none at all -- so the file is the subject of the
# check, and the environment is scrubbed of the variables `codex exec` would
# otherwise prefer over it.
verify_credential() {
	verify_message=$(mktemp)
	verify_log=$(mktemp)

	set +e
	env -u CODEX_API_KEY -u OPENAI_API_KEY -u CODEX_ACCESS_TOKEN \
		timeout 180 codex exec 'Reply with exactly: discobox-ok' \
		--skip-git-repo-check \
		--dangerously-bypass-approvals-and-sandbox \
		--output-last-message "$verify_message" >"$verify_log" 2>&1
	verify_status=$?
	set -e

	verify_reply=$(cat "$verify_message" 2>/dev/null || true)
	if [ "$verify_status" -ne 0 ] || [ -z "$verify_reply" ]; then
		echo
		echo "The credential did not work:" >&2
		# codex retries an unauthorized request several times; the tail carries
		# the actual reason without replaying the whole storm.
		tail -n 5 "$verify_log" >&2
		rm -f "$verify_message" "$verify_log"
		return 1
	fi
	rm -f "$verify_message" "$verify_log"
	echo "Codex replied: $verify_reply"
	return 0
}

# write_output records the result: the credential as a secret, plus the two
# files a sandbox needs to run codex with it. Secret shapes:
#   - keep previous:  usePrevious marker, no value (KEEP_PREVIOUS set).
#   - ChatGPT login:  the access token plus refresh material, type oauth.
#   - API key:        a plain token { token }.
write_output() {
	write_env=$1
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	CODEX_CONFIGURE_ENV_NAME="$write_env" \
		CODEX_CONFIGURE_NAME="$(env_label "$write_env")" \
		CODEX_CONFIGURE_TYPE="${OUTPUT_TYPE:-token}" \
		CODEX_CONFIGURE_TOKEN="${OUTPUT_TOKEN:-}" \
		CODEX_CONFIGURE_KEEP_PREVIOUS="${KEEP_PREVIOUS:-}" \
		CODEX_CONFIGURE_CAPTURE_FILE="${OAUTH_CAPTURE_FILE:-}" \
		CODEX_CONFIGURE_CONFIG_FILE="$CONFIG_FILE" \
		CODEX_CONFIGURE_CONFIG_PATH="$CONFIG_PATH" \
		CODEX_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		CODEX_CONFIGURE_API_KEY_ENV="$API_KEY_ENV" \
		CODEX_CONFIGURE_OAUTH_ENV="$OAUTH_ENV" \
		CODEX_CONFIGURE_LAST_REFRESH="$AUTH_LAST_REFRESH" \
		CODEX_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		CODEX_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const secret = {
			envName: process.env.CODEX_CONFIGURE_ENV_NAME,
			name: process.env.CODEX_CONFIGURE_NAME,
			type: process.env.CODEX_CONFIGURE_TYPE || 'token',
		};
		let capture = null;
		if (process.env.CODEX_CONFIGURE_KEEP_PREVIOUS) {
			secret.usePrevious = true;
		} else if (secret.type === 'oauth') {
			capture = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_CAPTURE_FILE, 'utf8'));
			secret.value = capture.secret;
		} else {
			secret.value = { token: process.env.CODEX_CONFIGURE_TOKEN };
		}
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}

		// Workspace trust is local runtime state, excluded from portable settings.
		const configFile = () => {
			let body = '';
			try {
				body = fs.readFileSync(process.env.CODEX_CONFIGURE_CONFIG_FILE, 'utf8');
			} catch (err) {
				body = '';
			}
			const path = require('path');
			const { project } = require(path.join(process.env.HARNESS_CAPTURE_DIR, 'harness-capture/settings.cjs'));
			const definition = require(path.join(process.env.HARNESS_CAPTURE_DIR, 'codex-image.json')).harness.saveBack;
			const settings = project(body, definition.settings.find(s => s.path === '.codex/config.toml')).content;
			return {
				path: process.env.CODEX_CONFIGURE_CONFIG_PATH,

				content: settings,
			};
		};

		// The credential is delivered as a file rather than an environment
		// variable: codex's interactive TUI reads no credential variable at all,
		// and a ChatGPT token has no variable to be read from in the first place.
		//
		// What lands in the file is the **sentinel**, not the credential: the
		// content is a reference, and each sandbox renders its own sentinel into
		// it. The proxy swaps it outbound, so no credential is written to a file
		// anywhere.
		//
		// Only the credential leaf is bound to a sandbox sentinel.
		const apiKeyAuthFile = () => ({
			path: process.env.CODEX_CONFIGURE_AUTH_PATH,

			content: JSON.stringify({
				OPENAI_API_KEY: { $secret: process.env.CODEX_CONFIGURE_API_KEY_ENV },
			}, null, 2) + '\n',
		});

		// The account the login belongs to travels as claims in an unsigned
		// id_token rather than as the signed one codex wrote. Codex needs the
		// claims — it addresses the ChatGPT backend with the account id and gates
		// on the plan — and never verifies the signature, so re-signing buys
		// nothing while keeping a signed identity assertion out of a harness file
		// that is not a secret.
		//
		// last_refresh is deliberately far future: codex must never rotate this
		// itself. The refresh token stays in the control plane, so a rotation
		// from inside a sandbox could not succeed, and does not need to — a
		// sentinel does not expire, and the control plane keeps the real token
		// fresh behind it. refresh_token is a placeholder for the same reason:
		// the field must be present, and it is never reachable.
		const { oauthAuthFile } = require(require('path').join(process.env.HARNESS_CAPTURE_DIR, 'codex-normalize.cjs'));

		const files = [configFile()];
		if (capture) {
			files.push(oauthAuthFile(capture.tokens));
		} else if (secret.usePrevious) {
			// Keeping the previous credential keeps the file it was captured
			// with: the account claims live there, and the seed hands it back.
			// Re-emitting it verbatim is what keeps it, since the output replaces
			// the configured file set wholesale.
			const kept = (previous.files || []).find((f) => f && f.path === process.env.CODEX_CONFIGURE_AUTH_PATH);
			if (kept) {
				files.push(kept);
			} else if (secret.envName === process.env.CODEX_CONFIGURE_API_KEY_ENV) {
				files.push(apiKeyAuthFile());
			}
		} else {
			files.push(apiKeyAuthFile());
		}
		fs.writeFileSync(process.env.CODEX_CONFIGURE_OUTPUT, JSON.stringify({ files, secrets: [secret] }));
	NODE_EOF
}

PREVIOUS_ENV=$(previous_env)

# Trust the workspace up front so the interactive launch below opens on the
# sign-in screen rather than the trust screen, whatever the config.toml a
# previous configure captured says.
ensure_workspace_trusted

ENV_NAME=""
TOKEN=""
OUTPUT_TYPE="token"
KEEP_PREVIOUS=""
SEEDED_SENTINEL=""

# Open the session already signed in when there is a credential to sign in with.
# There is no keep-or-replace question: the answer is whatever the user does in
# the session, and asking up front made changing a setting cost a sign-in.
seed_previous_credential

while [ -z "$ENV_NAME" ]; do
	printf '%s\n' "${C_BOLD}${C_WARN}==========================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up Codex — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}==========================================================${C_RESET}"
	echo
	echo "Discobox is about to start Codex so you can set it up. This is a"
	echo "throwaway setup sandbox: it exists only to capture your sign-in and"
	echo "settings, and it is deleted the moment you leave."
	printf '%s\n' "${C_BOLD}Do not start real work in here — none of it is kept.${C_RESET}"
	echo
	if [ -n "$SEEDED_SENTINEL" ]; then
		printf '%s\n' "${C_BOLD}You are already signed in ($(env_label "$PREVIOUS_ENV")).${C_RESET}"
		echo "Change whatever you like — settings, model, or the account itself."
		echo
		printf '%s\n' "  ${C_CMD}/model${C_RESET}      Pick the model this harness runs with"
		printf '%s\n' "  ${C_CMD}/theme${C_RESET}      Appearance, and ${C_CMD}/statusline${C_RESET} for the status line"
		printf '%s\n' "  ${C_CMD}/logout${C_RESET}     Only to switch accounts. Codex exits when you run it,"
		echo "              and setup offers to start it again so you can sign in."
		echo
		printf '%s\n' "${C_BOLD}Leave with ${C_CMD}/exit${C_RESET}${C_BOLD} (or Ctrl-D) when you're done — setup only"
		printf '%s\n' "finishes once you exit.${C_RESET} Your sign-in is kept unless you replace it."
	else
		printf '%s\n' "${C_BOLD}You must do both of these:${C_RESET}"
		echo
		printf '%s\n' "  1. ${C_BOLD}Sign in${C_RESET}  on the screen Codex opens on. Nothing can be saved"
		echo "              without it. This sandbox has no browser: for"
		printf '%s\n' "              ${C_BOLD}Sign in with ChatGPT${C_RESET}, open the link it prints on your own"
		echo "              machine — Discobox forwards the sign-in back in here."
		printf '%s\n' "              If Discobox warned that port 1455 is taken, pick"
		printf '%s\n' "              ${C_BOLD}Sign in with Device Code${C_RESET} instead — or use an API key."
		printf '%s\n' "  2. ${C_BOLD}${C_CMD}/exit${C_RESET}    Leave Codex when you're done. Setup only finishes"
		echo "              once you exit — staying in blocks it."
		echo
		echo "Worth doing while you're in there:"
		echo
		printf '%s\n' "  ${C_CMD}/model${C_RESET}      Pick the model this harness runs with"
		printf '%s\n' "  ${C_CMD}/theme${C_RESET}      Appearance, and ${C_CMD}/statusline${C_RESET} for the status line"
		echo
		printf '%s\n' "Then ${C_BOLD}${C_CMD}/exit${C_RESET} (or Ctrl-D) and Discobox will save your setup."
	fi
	echo
	confirm_launch

	# codex needs a real TTY for its interactive UI; run it under script.
	set +e
    # shellcheck disable=SC2016 # script's child shell expands the exported override.
	script -q -e -c 'codex -c "$DISCOBOX_CODEX_TRUST"' /dev/null
	codex_status=$?
	set -e
	echo

	if ! detect_credential; then
		if [ -n "$SEEDED_SENTINEL" ]; then
			# The session started signed in and came back with nothing, which is
			# what /logout leaves behind. Saying so points at the next step rather
			# than at a mistake.
			printf '%s\n' "${C_ERR}${C_BOLD}Codex is signed out. Start it again to sign in with another account.${C_RESET}" >&2
			SEEDED_SENTINEL=""
		elif [ "$codex_status" -ne 0 ]; then
			# Not "the user skipped the sign-in": codex refused to run at all, and
			# saying so points at the message it printed just above rather than
			# asking again as if a step had been missed.
			printf '%s\n' "${C_ERR}${C_BOLD}Codex exited with status $codex_status without configuring a credential.${C_RESET}" >&2
		else
			printf '%s\n' "${C_ERR}${C_BOLD}You left Codex without signing in, so there is nothing to" >&2
			printf '%s\n' "save. Setup needs you to sign in inside Codex before you exit.${C_RESET}" >&2
		fi
		confirm_retry
		continue
	fi

	if [ -n "$KEEP_PREVIOUS" ]; then
		echo "The credential is unchanged; checking it still works…"
	else
		echo "Verifying the credential…"
	fi
	if ! verify_credential; then
		echo
		if [ -n "$KEEP_PREVIOUS" ]; then
			# The existing credential is the thing that failed -- revoked, most
			# likely. Stop treating it as the baseline: another round would find
			# the same seeded sentinel and report "unchanged" again, offering a
			# retry that cannot succeed until the user signs in afresh.
			printf '%s\n' "${C_ERR}${C_BOLD}The existing credential no longer works. Sign in again to replace it.${C_RESET}" >&2
			SEEDED_SENTINEL=""
			KEEP_PREVIOUS=""
		fi
		clear_captured_credential
		[ -n "$OAUTH_CAPTURE_FILE" ] && rm -f "$OAUTH_CAPTURE_FILE"
		ENV_NAME=""
		TOKEN=""
		OUTPUT_TYPE="token"
		OAUTH_CAPTURE_FILE=""
		confirm_retry
	fi
done

if [ -n "$KEEP_PREVIOUS" ]; then
	OUTPUT_TOKEN="" write_output "$ENV_NAME"
else
	OUTPUT_TOKEN="$TOKEN" write_output "$ENV_NAME"
fi

[ -n "$OAUTH_CAPTURE_FILE" ] && rm -f "$OAUTH_CAPTURE_FILE"

echo
echo "Codex configuration complete."
