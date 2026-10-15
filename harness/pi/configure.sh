#!/bin/sh
# Runs as the configure sandbox's primary terminal (see "Configure flows"
# in harness/DESIGN.md). Launches pi interactively, lets the user sign in to
# whichever providers they want and set pi up however they normally would,
# then reads what pi itself stored to capture the result. Writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# pi keeps every credential in one file, ~/.pi/agent/auth.json, keyed by
# provider, and `/login` already offers every provider's own sign-in. So this
# script launches a bare `pi` and afterwards reads that file
# (ADR 26-10-15-259 §2):
#   - Each provider becomes one secret, PI_<PROVIDER>_CREDENTIAL.
#   - An API key is a `token` secret. One that is a command (`!…`) is not a
#     secret, and is left out.
#   - An OAuth login the control plane knows how to renew (see oauthRefresh
#     below) is an `oauth` secret with its refresh material.
#   - A GitHub Copilot login's secret is its GitHub token, which pi exchanges
#     for a short-lived Copilot token inside the sandbox.
#   - A ChatGPT subscription login is left out: pi reads the account from the
#     access token's claims, which no sentinel carries. So is a sign-in with
#     no key or token at all (Bedrock's AWS profile), which uses credentials
#     no sandbox has.
#   - Any other OAuth login is a `token` secret holding the access token, and
#     the script says when it expires, because nothing will renew it.
# The default model is checked with a one-line `pi -p`, and a failure is
# reported rather than refused (see verify_default_model).
#
# No credential is exported as an environment variable. The script returns
# auth.json as a templated harness file with each provider's sentinel in place
# of its secret — the same delivery the opencode image uses for its own.
#
# It also returns pi's settings as the user left them (settings.json: the
# default model, theme and the rest; keybindings.json), and Discobox's own
# settings for the harness. models.json and mcp.json are not returned: pi keeps
# literal keys and commands in both, which would become plaintext files.
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each one's value is
# available as $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real
# credential on the way out — so the previous auth.json is written back with
# sentinels and the session opens already signed in. A credential still
# holding its sentinel afterwards was not replaced, and is reported back as
# usePrevious rather than as a value.
set -eu

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json

# Home-relative, like every harness file.
AUTH_PATH=".pi/agent/auth.json"
PI_SETTINGS_PATHS=".pi/agent/settings.json .pi/agent/keybindings.json"
AUTH_FILE="$HOME/$AUTH_PATH"
# Discobox's own settings for this harness: the judge model. The image declares
# a baseline, and a configured harness delivers its own copy into this sandbox,
# which is what a reconfigure starts from.
SETTINGS_PATH=".config/discobox/pi-harness.json"

# The `expires` a delivered OAuth entry carries when the control plane renews
# its token: far enough out that pi never decides the credential is stale and
# tries a refresh itself. That is not a lie about the real token — what the
# entry carries is a sentinel, which genuinely does not go stale, and the
# refresh token it would need never leaves the control plane.
FAR_FUTURE_EXPIRY=4102444800000
REFRESH_PLACEHOLDER="discobox-refresh-happens-in-the-control-plane"
# The access token a delivered Copilot entry starts with. Its `expires` is
# zero, so pi exchanges the GitHub token in `refresh` for a real one before its
# first request, and never sends this.
COPILOT_ACCESS_PLACEHOLDER="discobox-copilot-token-is-exchanged-in-the-sandbox"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

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

# seed_previous_credentials writes, to $WORK/seed.json, the auth.json a
# previous run captured with each PREV_ sentinel in place of its template
# action, so the session below opens already signed in.
#
# This is what lets a reconfigure be about anything else. Reconfigure is usually
# about settings — a model, a theme, one more provider — and a flow that opened
# signed out of everything would make changing a setting cost every sign-in.
#
# What is written is the PREV_ sentinel, not the credential: the proxy swaps it
# on outbound requests, so the session is genuinely signed in while this
# sandbox never holds the real value. The sentinel is also how the change check
# works afterwards — it is a value we know, so finding it still in place means
# the credential was not replaced (see classify_credentials).
#
# A provider whose sentinel is not set — its secret was deleted, say — is left
# out, and opens signed out, which is the one case a sign-in is unavoidable.
seed_previous_credentials() {
	PI_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		PI_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		PI_CONFIGURE_SEED="$WORK/seed.json" \
		PI_CONFIGURE_SEEDED="$WORK/seeded.txt" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.PI_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const kept = (previous.files || []).find((f) => f && f.path === process.env.PI_CONFIGURE_AUTH_PATH);
		let template = {};
		try {
			template = JSON.parse(kept && typeof kept.content === 'string' ? kept.content : '{}') || {};
		} catch (err) {
			template = {};
		}
		const action = /\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;
		const seeded = {};
		for (const [provider, entry] of Object.entries(template)) {
			const text = JSON.stringify(entry);
			const names = [...text.matchAll(action)].map((m) => m[1]);
			if (names.length === 0 || names.some((name) => !process.env['PREV_' + name])) continue;
			seeded[provider] = JSON.parse(text.replace(action, (_, name) => process.env['PREV_' + name]));
		}
		if (Object.keys(seeded).length > 0) {
			fs.writeFileSync(process.env.PI_CONFIGURE_SEED, JSON.stringify(seeded, null, 2) + '\n', { mode: 0o600 });
		}
		fs.writeFileSync(process.env.PI_CONFIGURE_SEEDED, Object.keys(seeded).join(', '));
	NODE_EOF
	SEEDED_LABELS=$(cat "$WORK/seeded.txt")
}

# reset_stored_credentials leaves pi holding exactly the seeded credentials, so
# a retry's capture cannot mistake a sign-in from an earlier attempt in this
# same sandbox for a fresh one.
#
# A configured harness also delivers its auth.json into this sandbox, but
# rendered against secrets this sandbox does not have (they arrive
# PREV_-prefixed), so it describes credentials with nothing behind them; the
# seed replaces it.
reset_stored_credentials() {
	mkdir -p "$(dirname "$AUTH_FILE")"
	if [ -f "$WORK/seed.json" ]; then
		cp "$WORK/seed.json" "$AUTH_FILE"
		chmod 0600 "$AUTH_FILE"
	else
		rm -f "$AUTH_FILE"
	fi
}

# classify_credentials turns auth.json into $WORK/plan.json: one entry per
# provider, with the secret to store (or the note that the previous one is
# kept) and the auth.json entry that delivers it, and the providers left out
# with why. Prints how many secrets there are.
#
# A credential whose secret material is still the PREV_ sentinel seeded for its
# name was not replaced, and is kept with the entry the last run returned —
# which carries whatever the real sign-in recorded, and which the seed was
# rendered from. Anything else is a new credential and is captured.
classify_credentials() {
	PI_CONFIGURE_AUTH_FILE="$AUTH_FILE" \
		PI_CONFIGURE_PLAN="$WORK/plan.json" \
		PI_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		PI_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		PI_CONFIGURE_FAR_FUTURE_EXPIRY="$FAR_FUTURE_EXPIRY" \
		PI_CONFIGURE_REFRESH_PLACEHOLDER="$REFRESH_PLACEHOLDER" \
		PI_CONFIGURE_COPILOT_ACCESS_PLACEHOLDER="$COPILOT_ACCESS_PLACEHOLDER" node <<-'NODE_EOF'
		const fs = require('fs');
		let auth = {};
		try {
			auth = JSON.parse(fs.readFileSync(process.env.PI_CONFIGURE_AUTH_FILE, 'utf8')) || {};
		} catch (err) {
			auth = {};
		}
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.PI_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const farFuture = Number(process.env.PI_CONFIGURE_FAR_FUTURE_EXPIRY);
		const placeholder = process.env.PI_CONFIGURE_REFRESH_PLACEHOLDER;
		const copilotAccess = process.env.PI_CONFIGURE_COPILOT_ACCESS_PLACEHOLDER;

		// The previous run's entries and secrets, by the env name each entry's
		// template action names.
		const action = /\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/;
		const previousEntries = new Map();
		const kept = (previous.files || []).find((f) => f && f.path === process.env.PI_CONFIGURE_AUTH_PATH);
		try {
			for (const entry of Object.values(JSON.parse(kept ? kept.content : '{}') || {})) {
				const match = JSON.stringify(entry).match(action);
				if (match) previousEntries.set(match[1], entry);
			}
		} catch (err) {
			// A previous file that does not parse keeps nothing.
		}
		const previousSecrets = new Map((previous.secrets || []).map((s) => [s && s.envName, s || {}]));

		// oauthRefresh: the providers whose OAuth login the control plane can
		// renew, and how. Each is a property of that provider's public OAuth
		// client — its token endpoint, client id, and request encoding — not of
		// any one sign-in, so they are fixed here rather than read from the
		// file. pi signs in to these with the same clients the claude-code image
		// (and opencode's xAI) are refreshed for. A provider not listed is
		// delivered as a plain token.
		const oauthRefresh = {
			anthropic: { tokenUrl: 'https://console.anthropic.com/v1/oauth/token', clientId: '9d1c250a-e61b-44d9-88ed-5944d1962f5e' },
			// xAI's token endpoint takes the refresh request form-encoded.
			xai: { tokenUrl: 'https://auth.x.ai/oauth2/token', clientId: 'b1a00492-073a-47ea-816f-4c329264a828', tokenRequestEncoding: 'form' },
		};

		const envNameFor = (provider) => 'PI_' + provider.toUpperCase().replace(/[^A-Z0-9]+/g, '_').replace(/^_+|_+$/g, '') + '_CREDENTIAL';

		// notice says when a login nothing renews stops working, or that it
		// already has: a kept one as much as a new one, since a reconfigure
		// about something else would otherwise carry a dead login on in
		// silence.
		const notice = (provider, credential) => {
			if (!credential || credential.type !== 'oauth') return undefined;
			const expires = Number(credential.expires) || 0;
			if (expires <= 0 || expires >= farFuture) return undefined;
			const day = new Date(expires).toISOString().slice(0, 10);
			if (expires <= Date.now()) {
				return `${provider}'s sign-in expired ${day}, and Discobox cannot renew it. Sign in to it again, or with an API key.`;
			}
			return `${provider}'s sign-in expires ${day}, and Discobox cannot renew it. Configure again after that, or sign in with an API key.`;
		};

		// copilotHost is the GitHub API host a Copilot login's GitHub token is
		// exchanged at, and so the one host its secret is sent to:
		// api.github.com, or api.<domain> for GitHub Enterprise.
		const copilotHost = (credential) => {
			const domain = typeof credential.enterpriseUrl === 'string' && credential.enterpriseUrl
				? credential.enterpriseUrl.replace(/^[a-z]+:\/\//i, '').replace(/\/.*$/, '')
				: 'github.com';
			return `api.${domain}`;
		};

		const plan = [];
		const skipped = [];
		for (const provider of Object.keys(auth).sort()) {
			const value = auth[provider];
			if (!value || typeof value !== 'object') continue;
			const copilot = provider === 'github-copilot' && value.type === 'oauth';
			// What the secret is: an API key, an OAuth login's access token, or
			// a Copilot login's GitHub token, which pi keeps as its refresh
			// token.
			const material = value.type === 'api_key' ? value.key : copilot ? value.refresh : value.type === 'oauth' ? value.access : undefined;
			if (typeof material !== 'string' || !material) {
				// A sign-in with no key or token of its own — Bedrock's AWS
				// profile or credential chain — uses credentials this machine
				// keeps, which no sandbox has.
				skipped.push(`${provider} has no key or token of its own (it uses credentials kept on the machine pi runs on), so it is not kept. Sign in to it with a key instead.`);
				continue;
			}
			const envName = envNameFor(provider);
			const sentinel = process.env['PREV_' + envName];
			if (sentinel && material === sentinel && previousEntries.has(envName)) {
				const secret = previousSecrets.get(envName) || {};
				const entry = previousEntries.get(envName);
				const type = secret.type || 'token';
				const item = { envName, provider, kept: true, type, entry };
				if (secret.host) item.host = secret.host;
				if (type === 'token' && !copilot) item.notice = notice(provider, entry);
				plan.push(item);
				continue;
			}
			if (value.type === 'api_key' && material.startsWith('!')) {
				skipped.push(`${provider}'s key is a command (${material.split(/\s/)[0]}…), which runs where pi runs, so it is not kept. Sign in with the key itself.`);
				continue;
			}
			if (provider === 'openai-codex' && value.type === 'oauth') {
				skipped.push(`openai-codex (a ChatGPT subscription) is not kept: pi reads the account from the sign-in's own token, which Discobox never hands a sandbox. Sign in to OpenAI with an API key instead.`);
				continue;
			}
			// Dotted field access, never a quoted map key: this content is JSON,
			// so a quote inside a template action is escaped into the file itself
			// and Go's template parser rejects it — which breaks every sandbox
			// launch while configure still reports success. An env name is
			// always a valid template field name, so there is nothing to quote.
			const template = `{{ .secrets.${envName} }}`;
			const item = { envName, provider, kept: false };
			if (value.type === 'api_key') {
				item.type = 'token';
				item.value = { token: value.key };
				item.entry = { ...value, key: template };
			} else if (copilot) {
				// The GitHub token alone is the secret, bound to the one host it
				// is exchanged at. The Copilot token it buys lasts half an hour,
				// so pi exchanges the sentinel for a fresh one inside the
				// sandbox, through the proxy, before its first request.
				item.type = 'token';
				item.host = copilotHost(value);
				item.value = { token: value.refresh };
				item.entry = { ...value, access: copilotAccess, refresh: template, expires: 0 };
			} else if (value.type === 'oauth') {
				const spec = oauthRefresh[provider];
				if (spec && value.refresh && value.refresh !== value.access) {
					item.type = 'oauth';
					item.value = { token: value.access, refreshToken: value.refresh, tokenUrl: spec.tokenUrl, clientId: spec.clientId };
					if (spec.tokenRequestEncoding) item.value.tokenRequestEncoding = spec.tokenRequestEncoding;
					if (Number(value.expires) > 0) item.value.accessTokenExpiresAt = Number(value.expires);
					// The refresh token stays in the control plane, so a refresh
					// from inside a sandbox could not succeed — and does not need
					// to, since a sentinel does not go stale.
					item.entry = { ...value, access: template, refresh: placeholder, expires: farFuture };
				} else {
					item.type = 'token';
					item.value = { token: value.access };
					item.entry = {
						...value,
						access: template,
						refresh: value.refresh === value.access ? template : (value.refresh ? placeholder : value.refresh),
					};
					item.notice = notice(provider, value);
				}
			} else {
				continue;
			}
			plan.push(item);
		}
		fs.writeFileSync(process.env.PI_CONFIGURE_PLAN, JSON.stringify({ plan, skipped }));
		process.stdout.write(String(plan.length));
	NODE_EOF
}

# plan_labels prints the providers in the plan, comma-separated.
plan_labels() {
	PI_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const { plan } = JSON.parse(require("fs").readFileSync(process.env.PI_CONFIGURE_PLAN, "utf8"));
		process.stdout.write(plan.map((p) => p.provider).join(", "));
	'
}

# verify_default_model runs a trivial, tool-free prompt on the model a
# sandbox's pi starts with — `pi -p` given no --model uses pi's default: the
# default model in its settings, else pi's pick among the signed-in providers.
# It runs from an empty directory, so no project's settings choose instead.
#
# Only that model is checked, and a failure is a warning rather than a gate.
# Whether a request succeeds depends on the model as much as the credential —
# a plan that excludes it, a region it needs opting in to — and pi's model list
# says nothing of either, so checking every credential on a model picked for it
# would report working credentials as broken. What a failure does show is that
# the harness's first session would fail the same way, which is worth fixing
# while pi is one keystroke away.
verify_default_model() {
	mkdir -p "$WORK/verify"
	set +e
	(cd "$WORK/verify" && timeout 180 pi -p --no-tools --no-session -- 'Reply with exactly: discobox-ok' \
		</dev/null >"$WORK/verify.log" 2>&1)
	verify_status=$?
	set -e
	if [ "$verify_status" -eq 0 ] && grep -qi 'discobox-ok' "$WORK/verify.log"; then
		return 0
	fi
	tail -n 5 "$WORK/verify.log" >&2
	return 1
}

# print_notices shows what the plan says about credentials left out, and those
# nothing will renew.
print_notices() {
	PI_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const { plan, skipped } = JSON.parse(require("fs").readFileSync(process.env.PI_CONFIGURE_PLAN, "utf8"));
		for (const line of skipped) process.stdout.write(line + "\n");
		for (const item of plan) if (item.notice) process.stdout.write(item.notice + "\n");
	'
}

# confirm_launch holds the instructions on screen until the user is ready. pi
# draws a full-screen TUI, so anything printed right before launching it is
# gone before it can be read. Waiting for Enter means the banner is read while
# it is still the only thing on screen.
#
# End of input is not "yes": nobody is there, and launching an interactive TUI
# at nobody wedges the configure flow rather than failing it.
confirm_launch() {
	printf '%s' "${C_BOLD}Press Enter to start pi.${C_RESET} "
	if ! read -r _launch_ack; then
		echo >&2
		echo "No input; aborting without configuring pi." >&2
		exit 1
	fi
	echo
}

# confirm_retry asks whether to launch pi again, and says what answering no
# does, since here it is not always "abort". Every retry goes through here so
# the loop can only turn when a person asks it to: an attempt that fails without
# reaching the user — pi refusing to start, say — fails again the moment it is
# retried, and looping on that is a busy loop, not a retry. End of input means
# nobody is there to answer, which fails the configure flow rather than
# spinning.
confirm_retry() {
	printf 'Start pi again? Answering n %s. [Y/n] ' "$1"
	if ! read -r retry_choice; then
		echo >&2
		echo "No input; aborting without configuring pi." >&2
		exit 1
	fi
	echo
	case "${retry_choice:-y}" in
	[nN]*) return 1 ;;
	esac
	return 0
}

# write_output records the result: one secret per provider, the auth.json that
# delivers them, pi's settings as the user left them, and Discobox's settings
# for the harness. Secret shapes:
#   - kept:          usePrevious marker, no value.
#   - OAuth renewed: the access token plus refresh material, type oauth.
#   - anything else: a plain token, bound to a host when it is a Copilot
#                    login's GitHub token.
write_output() {
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	PI_CONFIGURE_PLAN="$WORK/plan.json" \
		PI_CONFIGURE_HOME="$HOME" \
		PI_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		PI_CONFIGURE_PI_SETTINGS_PATHS="$PI_SETTINGS_PATHS" \
		PI_CONFIGURE_SETTINGS_PATH="$SETTINGS_PATH" \
		PI_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const path = require('path');
		const home = process.env.PI_CONFIGURE_HOME;
		const { plan } = JSON.parse(fs.readFileSync(process.env.PI_CONFIGURE_PLAN, 'utf8'));
		const secrets = plan.map((item) => {
			const secret = { envName: item.envName, name: `pi: ${item.provider}`, type: item.type };
			if (item.host) secret.host = item.host;
			if (item.kept) {
				secret.usePrevious = true;
			} else {
				secret.value = item.value;
			}
			return secret;
		});
		const files = [];
		if (plan.length > 0) {
			// The sentinel lands in the file, not the credential: the content is
			// a template, and each sandbox renders its own sentinels into it.
			const auth = {};
			for (const item of plan) auth[item.provider] = item.entry;
			files.push({
				path: process.env.PI_CONFIGURE_AUTH_PATH,
				template: true,
				content: JSON.stringify(auth, null, 2) + '\n',
			});
		}
		// pi's settings are returned literally, as pi wrote them: they are the
		// user's.
		for (const rel of process.env.PI_CONFIGURE_PI_SETTINGS_PATHS.split(' ').filter(Boolean)) {
			try {
				files.push({ path: rel, content: fs.readFileSync(path.join(home, rel), 'utf8') });
			} catch (err) {
				// Not written in this session or any before it; nothing to keep.
			}
		}
		// Discobox's settings, as they arrived — a judge model someone set by
		// editing the file is theirs.
		const settingsPath = process.env.PI_CONFIGURE_SETTINGS_PATH;
		let settings = {};
		try {
			settings = JSON.parse(fs.readFileSync(path.join(home, settingsPath), 'utf8')) || {};
		} catch (err) {
			settings = {};
		}
		if (typeof settings.judgeModel !== 'string') settings.judgeModel = '';
		files.push({ path: settingsPath, content: JSON.stringify(settings, null, 2) + '\n' });
		fs.writeFileSync(process.env.PI_CONFIGURE_OUTPUT, JSON.stringify({ files, secrets }));
	NODE_EOF
}

SEEDED_LABELS=""
seed_previous_credentials

while :; do
	reset_stored_credentials

	printf '%s\n' "${C_BOLD}${C_WARN}=======================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up pi — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}=======================================================${C_RESET}"
	echo
	echo "Discobox is about to start pi so you can set it up. This is a"
	echo "throwaway setup sandbox: it exists only to capture your sign-ins and"
	echo "settings, and it is deleted the moment you leave."
	printf '%s\n' "${C_BOLD}Do not start real work in here — none of it is kept.${C_RESET}"
	echo
	if [ -n "$SEEDED_LABELS" ]; then
		printf '%s\n' "${C_BOLD}Already signed in: $SEEDED_LABELS.${C_RESET}"
		echo "Change whatever you like — settings, the model, or the providers."
		echo
	fi
	printf '%s\n' "${C_BOLD}You must do both of these:${C_RESET}"
	echo
	printf '%s\n' "  1. ${C_BOLD}${C_CMD}/login${C_RESET}   Sign in to each provider this harness should use —"
	echo "             as many as you like. This sandbox has no browser: a"
	echo "             sign-in prints a link to open on your own machine."
	echo "             If its page cannot reach back in here, copy the address"
	echo "             your browser lands on and paste it into pi."
	echo "             A ChatGPT subscription cannot be used here; use an"
	echo "             OpenAI API key."
	if [ -n "$SEEDED_LABELS" ]; then
		echo "             (Optional now: providers you already signed in to are kept.)"
	fi
	printf '%s\n' "  2. ${C_BOLD}${C_CMD}/quit${C_RESET}    Leave pi when you're done. Setup only finishes"
	echo "             once you quit — staying in blocks it."
	echo
	echo "Worth doing while you're in there:"
	echo
	printf '%s\n' "  ${C_CMD}/model${C_RESET}     Pick the default model this harness starts with: in the"
	echo "             picker, use the key it shows to set it as the default"
	echo "             (/model NAME changes this session's model only)"
	printf '%s\n' "  ${C_CMD}/settings${C_RESET}  Theme, thinking level, and the rest"
	echo
	confirm_launch

	# pi needs a real TTY for its interactive UI; run it under script.
	set +e
	script -q -e -c 'pi' /dev/null
	pi_status=$?
	set -e
	# pi can leave its full-screen UI's contents behind when it exits, and
	# everything printed next would be drawn over them.
	# Only after a clean exit: when pi fails, what it printed is the
	# explanation, and clearing would erase it.
	if [ "$pi_status" -eq 0 ] && [ -t 1 ]; then
		printf '\033[?1049l\033[H\033[2J'
	fi
	echo

	count=$(classify_credentials)

	if [ "$count" -eq 0 ]; then
		# Said now, before the choice: a provider left out may be why nothing
		# is signed in.
		print_notices
		if [ "$pi_status" -ne 0 ]; then
			# Not "the user signed in to nothing": pi refused to run at all,
			# and saying so points at the message it printed just above.
			printf '%s\n' "${C_ERR}${C_BOLD}pi exited with status $pi_status without signing in to a provider.${C_RESET}" >&2
		else
			printf '%s\n' "${C_ERR}${C_BOLD}No provider is signed in.${C_RESET}" >&2
			echo "pi still runs without one — with a local model server, or a provider"
			echo "signed in inside a sandbox — but this harness will hold no credentials."
		fi
		if confirm_retry "saves your settings with no provider"; then
			continue
		fi
		break
	fi

	echo "Signed in: $(plan_labels). Checking the default model…"
	if verify_default_model; then
		echo "The default model answered."
		break
	fi
	echo
	printf '%s\n' "${C_ERR}${C_BOLD}pi's default model did not answer.${C_RESET}" >&2
	echo "Your sign-ins are kept. Start pi again to pick another model with"
	echo "/model, or sign in again with /login."
	if confirm_retry "saves everything as it is"; then
		# The sign-ins this round made are the next round's starting point,
		# so starting again does not cost them.
		cp "$AUTH_FILE" "$WORK/seed.json"
		SEEDED_LABELS=$(plan_labels)
		continue
	fi
	break
done

if [ "$count" -gt 0 ]; then
	print_notices
fi
write_output

echo
echo "pi configuration complete."
