#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# codex-cli Definition). Launches Codex interactively, lets the user sign in and
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
#     stored as a plain `bearer` secret (OPENAI_API_KEY).
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
OAUTH_TOKEN_URL="https://auth.openai.com/oauth/token"
OAUTH_CLIENT_ID="app_EMoamEEZ73f0CkXaXp7hrann"

# The last_refresh written into the delivered auth.json: far enough out that
# codex never decides the credential is stale and rotates it itself. This is not
# a lie about the real token — what the file carries is a sentinel, which
# genuinely does not go stale, and the control plane refreshes the credential
# behind it. Codex rotates when last_refresh is older than 28 days, so a future
# date simply never triggers.
AUTH_LAST_REFRESH="2100-01-01T00:00:00Z"

# The workspace the configure sandbox runs in. Unlike a run sandbox it has no
# source, so the image's config.toml template trusts nothing — we mark the
# workspace trusted below so `codex` opens straight into onboarding rather than
# stopping at the trust screen.
WORKSPACE_DIR="${DISCOBOX_WORKING_ROOT:-/workspace}"

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

# ensure_workspace_trusted marks the workspace (and the current directory) as
# trusted in ~/.codex/config.toml, so the interactive `codex` launch below opens
# on the sign-in screen instead of the trust screen. It rewrites only the
# [projects] tables — the same ones write_output strips back out, since this
# sandbox's trust map must not become the harness's.
ensure_workspace_trusted() {
	CODEX_CONFIGURE_CONFIG_FILE="$CONFIG_FILE" \
		CODEX_CONFIGURE_TRUST_DIRS="$WORKSPACE_DIR
$PWD" node <<-'NODE_EOF'
		const fs = require('fs');
		const nodePath = require('path');
		const file = process.env.CODEX_CONFIGURE_CONFIG_FILE;
		let body = '';
		try {
			body = fs.readFileSync(file, 'utf8');
		} catch (err) {
			body = '';
		}
		// Dropping the existing [projects] tables first keeps this idempotent:
		// TOML rejects a table defined twice, so a second run that simply
		// appended would leave codex unable to read its own config.
		const kept = [];
		let dropping = false;
		for (const line of body.split('\n')) {
			const header = line.match(/^\s*\[\[?\s*([^\]]*?)\s*\]\]?\s*$/);
			if (header) {
				const name = header[1];
				dropping = name === 'projects' || name.startsWith('projects.');
				if (dropping) continue;
			} else if (/^\s*projects\s*=/.test(line)) {
				continue;
			}
			if (dropping) continue;
			kept.push(line);
		}
		const dirs = [];
		for (const dir of process.env.CODEX_CONFIGURE_TRUST_DIRS.split('\n')) {
			const trimmed = (dir || '').trim();
			if (trimmed && !dirs.includes(trimmed)) dirs.push(trimmed);
		}
		let out = kept.join('\n').replace(/\s+$/, '');
		for (const dir of dirs) {
			out += `\n\n[projects.${JSON.stringify(dir)}]\ntrust_level = "trusted"`;
		}
		fs.mkdirSync(nodePath.dirname(file), { recursive: true });
		fs.writeFileSync(file, out.replace(/^\n+/, '') + '\n');
	NODE_EOF
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
		CODEX_CONFIGURE_TOKEN_URL="$OAUTH_TOKEN_URL" \
		CODEX_CONFIGURE_CLIENT_ID="$OAUTH_CLIENT_ID" \
		CODEX_CONFIGURE_CAPTURE_FILE="$OAUTH_CAPTURE_FILE" node <<-'NODE_EOF'
		const fs = require('fs');
		let auth = {};
		try {
			auth = JSON.parse(fs.readFileSync(process.env.CODEX_CONFIGURE_AUTH_FILE, 'utf8')) || {};
		} catch (err) {
			auth = {};
		}
		const tokens = auth.tokens || {};
		const accessToken = tokens.access_token;
		const refreshToken = tokens.refresh_token;
		if (typeof accessToken !== 'string' || !accessToken ||
			typeof refreshToken !== 'string' || !refreshToken) {
			process.exit(3);
		}
		const decodeClaims = (jwt) => {
			if (typeof jwt !== 'string') return null;
			const parts = jwt.split('.');
			if (parts.length !== 3 || !parts[1]) return null;
			try {
				return JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8'));
			} catch (err) {
				return null;
			}
		};
		const secret = {
			token: accessToken,
			refreshToken,
			tokenUrl: process.env.CODEX_CONFIGURE_TOKEN_URL,
			clientId: process.env.CODEX_CONFIGURE_CLIENT_ID,
		};
		// The access token is a JWT, so its own expiry is on hand. Recording it
		// is what lets the control plane refresh just ahead of expiry instead of
		// discovering it from a 401.
		const accessClaims = decodeClaims(accessToken);
		if (accessClaims && typeof accessClaims.exp === 'number') {
			secret.accessTokenExpiresAt = accessClaims.exp * 1000;
		}
		const idClaims = decodeClaims(tokens.id_token) || {};
		const authClaims = idClaims['https://api.openai.com/auth'] || {};
		if (typeof authClaims.chatgpt_plan_type === 'string' && authClaims.chatgpt_plan_type) {
			secret.subscriptionType = authClaims.chatgpt_plan_type;
		}
		const account = {};
		const email = idClaims.email || (idClaims['https://api.openai.com/profile'] || {}).email;
		if (typeof email === 'string' && email) account.email = email;
		for (const claim of ['chatgpt_plan_type', 'chatgpt_user_id', 'chatgpt_account_id']) {
			if (typeof authClaims[claim] === 'string' && authClaims[claim]) account[claim] = authClaims[claim];
		}
		if (authClaims.chatgpt_account_is_fedramp === true) account.chatgpt_account_is_fedramp = true;
		const accountId = typeof tokens.account_id === 'string' && tokens.account_id
			? tokens.account_id
			: (account.chatgpt_account_id || null);
		fs.writeFileSync(process.env.CODEX_CONFIGURE_CAPTURE_FILE,
			JSON.stringify({ secret, tokens: { account, accountId } }));
		process.stdout.write(accessToken);
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
# place of its template action, so it carries the same account claims the real
# login had. A configuration made before this flow returned that file has
# nothing to replay and simply opens signed out, which is the one case where a
# sign-in is unavoidable.
#
# Whatever is not seeded is removed. A configured harness delivers its auth.json
# into this sandbox like any other, but its template renders against secrets
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
		CODEX_CONFIGURE_SENTINEL="$SEEDED_SENTINEL" node <<-'NODE_EOF'
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
		const content = kept.content.replace(/\{\{[^}]*\}\}/g, process.env.CODEX_CONFIGURE_SENTINEL);
		try {
			JSON.parse(content);
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
		OUTPUT_TYPE="bearer"
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
			OUTPUT_TYPE="bearer"
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
#   - API key:        a plain bearer { token }.
write_output() {
	write_env=$1
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	CODEX_CONFIGURE_ENV_NAME="$write_env" \
		CODEX_CONFIGURE_NAME="$(env_label "$write_env")" \
		CODEX_CONFIGURE_TYPE="${OUTPUT_TYPE:-bearer}" \
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
			type: process.env.CODEX_CONFIGURE_TYPE || 'bearer',
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

		// Codex keeps settings and directory trust in one file, so returning it
		// verbatim would make this throwaway sandbox's trust map the harness's.
		// The [projects] tables are dropped and one templated stanza put back,
		// which is the same thing the image's own config.toml declares: trust
		// the sandbox's primary source, whatever path it lands on.
		const trustStanza = [
			'',
			'{{- range .sources }}{{- if eq .slug "primary" }}',
			'[projects.{{ .target | json }}]',
			'trust_level = "trusted"',
			'{{- end }}{{- end }}',
			'',
		].join('\n');
		const configFile = () => {
			let body = '';
			try {
				body = fs.readFileSync(process.env.CODEX_CONFIGURE_CONFIG_FILE, 'utf8');
			} catch (err) {
				body = '';
			}
			// A line scanner, not a TOML parser: the only thing being removed is
			// a table, and a table ends where the next header begins. A [projects]
			// table written as a single-line inline value is dropped too; one
			// spanning several lines is not, and codex does not write one.
			const kept = [];
			let dropping = false;
			for (const line of body.split('\n')) {
				const header = line.match(/^\s*\[\[?\s*([^\]]*?)\s*\]\]?\s*$/);
				if (header) {
					const name = header[1];
					dropping = name === 'projects' || name.startsWith('projects.');
					if (dropping) continue;
				} else if (/^\s*projects\s*=/.test(line)) {
					continue;
				}
				if (dropping) continue;
				kept.push(line);
			}
			const settings = kept.join('\n').replace(/\s+$/, '');
			return {
				path: process.env.CODEX_CONFIGURE_CONFIG_PATH,
				template: true,
				content: `${settings}\n${trustStanza}`.replace(/^\n+/, ''),
			};
		};

		// The credential is delivered as a file rather than an environment
		// variable: codex's interactive TUI reads no credential variable at all,
		// and a ChatGPT token has no variable to be read from in the first place.
		//
		// What lands in the file is the **sentinel**, not the credential: the
		// content is a template, and each sandbox renders its own sentinel into
		// it. The proxy swaps it outbound, so no credential is written to a file
		// anywhere.
		//
		// Dotted field access, never a quoted map key: this content is JSON, so a
		// quote inside a template action is escaped into the file itself and Go's
		// template parser rejects it ("unexpected \\ in operand") — which breaks
		// every sandbox launch while configure still reports success. A secret's
		// env name is always a valid template field name
		// (HarnessConfigEnvVarNamePattern), so there is nothing to quote.
		const apiKeyAuthFile = () => ({
			path: process.env.CODEX_CONFIGURE_AUTH_PATH,
			template: true,
			content: JSON.stringify({
				OPENAI_API_KEY: `{{ .secrets.${process.env.CODEX_CONFIGURE_API_KEY_ENV} }}`,
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
		const oauthAuthFile = (tokens) => {
			const b64url = (value) => Buffer.from(JSON.stringify(value), 'utf8').toString('base64url');
			const claims = {};
			if (tokens.account.email) claims.email = tokens.account.email;
			const authClaims = {};
			for (const claim of ['chatgpt_plan_type', 'chatgpt_user_id', 'chatgpt_account_id']) {
				if (tokens.account[claim]) authClaims[claim] = tokens.account[claim];
			}
			if (tokens.account.chatgpt_account_is_fedramp) authClaims.chatgpt_account_is_fedramp = true;
			claims['https://api.openai.com/auth'] = authClaims;
			const idToken = `${b64url({ alg: 'none', typ: 'JWT' })}.${b64url(claims)}.discobox`;
			return {
				path: process.env.CODEX_CONFIGURE_AUTH_PATH,
				template: true,
				content: JSON.stringify({
					OPENAI_API_KEY: null,
					tokens: {
						id_token: idToken,
						access_token: `{{ .secrets.${process.env.CODEX_CONFIGURE_OAUTH_ENV} }}`,
						refresh_token: 'discobox-refresh-happens-in-the-control-plane',
						account_id: tokens.accountId,
					},
					last_refresh: process.env.CODEX_CONFIGURE_LAST_REFRESH,
				}, null, 2) + '\n',
			};
		};

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
# sign-in screen rather than the trust screen.
ensure_workspace_trusted

ENV_NAME=""
TOKEN=""
OUTPUT_TYPE="bearer"
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
	script -q -e -c 'codex' /dev/null
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
		OUTPUT_TYPE="bearer"
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
