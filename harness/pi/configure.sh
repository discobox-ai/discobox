#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# pi Definition). Launches pi interactively, lets the user sign in to whichever
# providers they want and configure it however they normally would, then reads
# what pi itself stored to capture the result. Writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# pi keeps every provider credential in one file, <agent dir>/auth.json, keyed
# by provider, and `/login` already offers every provider's own sign-in — API
# keys, and the OAuth sign-ins some providers have (Claude Pro/Max, ChatGPT,
# GitHub Copilot, ...). So this script launches a bare `pi` and afterwards
# reads that file:
#   - Each provider becomes one secret, PI_<PROVIDER>_CREDENTIAL.
#   - An API key is a `token` secret.
#   - An OAuth sign-in the control plane knows how to renew (see oauthRefresh
#     below) is an `oauth` secret with its refresh material.
#   - Any other OAuth sign-in is a `token` secret holding the access token, and
#     the script says when it expires, because nothing will renew it.
# The default model is checked with a one-line `pi --print`, and a failure is
# reported rather than refused (see verify_default_model).
#
# No credential is exported as an environment variable. The script returns
# auth.json as a templated harness file with each provider's sentinel in place
# of its secret — the same delivery the codex and opencode images use for their
# own auth.json.
#
# It also returns pi's settings as the user left them — settings.json (model,
# theme, thinking level), keybindings.json, and models.json (custom providers)
# — so they become the harness's defaults.
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

# Home-relative, like every harness file. pi's agent directory is ~/.pi/agent
# unless PI_CODING_AGENT_DIR moves it, which nothing in this image does.
AUTH_PATH=".pi/agent/auth.json"
SETTINGS_PATHS=".pi/agent/settings.json .pi/agent/keybindings.json .pi/agent/models.json"
AUTH_FILE="$HOME/$AUTH_PATH"
# Discobox's own settings for this harness: the judge model. The image declares
# a baseline, and a configured harness delivers its own copy into this
# sandbox, which is what a reconfigure starts from.
SETTINGS_PATH=".config/discobox/pi-harness.json"

# The `expires` a delivered OAuth entry carries when the control plane renews
# its token: far enough out that pi never decides the credential is stale and
# tries a refresh itself. That is not a lie about the real token — what the
# entry carries is a sentinel, which genuinely does not go stale, and the
# refresh token it would need never leaves the control plane.
FAR_FUTURE_EXPIRY=4102444800000
REFRESH_PLACEHOLDER="discobox-refresh-happens-in-the-control-plane"

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

# seed_previous_credentials writes the auth.json a previous run captured back
# where pi reads it, so the session below opens already signed in.
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
#
# Whatever is not seeded is removed. A configured harness delivers its auth.json
# into this sandbox like any other file, but its template renders against
# secrets this sandbox does not have (they arrive PREV_-prefixed), so what lands
# describes credentials with nothing behind them.
seed_previous_credentials() {
	PI_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		PI_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		PI_CONFIGURE_AUTH_FILE="$AUTH_FILE" \
		PI_CONFIGURE_SEEDED="$WORK/seeded.txt" node <<-'NODE_EOF'
		const fs = require('fs');
		const nodePath = require('path');
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
		const file = process.env.PI_CONFIGURE_AUTH_FILE;
		if (Object.keys(seeded).length === 0) {
			fs.rmSync(file, { force: true });
		} else {
			fs.mkdirSync(nodePath.dirname(file), { recursive: true, mode: 0o700 });
			fs.writeFileSync(file, JSON.stringify(seeded, null, 2) + '\n', { mode: 0o600 });
		}
		fs.writeFileSync(process.env.PI_CONFIGURE_SEEDED, Object.keys(seeded).join(', '));
	NODE_EOF
	SEEDED_LABELS=$(cat "$WORK/seeded.txt")
}

# classify_credentials turns auth.json into $WORK/plan.json: one entry per
# provider, with the secret to store (or the note that the previous one is
# kept) and the auth.json entry that delivers it. Prints how many there are.
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
		PI_CONFIGURE_REFRESH_PLACEHOLDER="$REFRESH_PLACEHOLDER" node <<-'NODE_EOF'
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

		// The previous run's entries and secret types, by the env name each
		// entry's template action names.
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
		const previousTypes = new Map((previous.secrets || []).map((s) => [s && s.envName, s && s.type]));

		// oauthRefresh: the providers whose OAuth sign-in the control plane can
		// renew, and how. Each is a property of that provider's public OAuth
		// client — its token endpoint, client id, and request encoding — not of
		// any one sign-in, so they are fixed here rather than read from the file;
		// they are the endpoints and clients pi's own refresh uses. A provider
		// not listed is delivered as a plain token.
		const oauthRefresh = {
			// The Claude Pro/Max sign-in: Claude Code's own client, at the
			// endpoint pi refreshes it through.
			anthropic: { tokenUrl: 'https://platform.claude.com/v1/oauth/token', clientId: '9d1c250a-e61b-44d9-88ed-5944d1962f5e' },
			// The same client and endpoint the codex image refreshes as JSON.
			'openai-codex': { tokenUrl: 'https://auth.openai.com/oauth/token', clientId: 'app_EMoamEEZ73f0CkXaXp7hrann' },
			// xAI's token endpoint takes the refresh request form-encoded.
			xai: { tokenUrl: 'https://auth.x.ai/oauth2/token', clientId: 'b1a00492-073a-47ea-816f-4c329264a828', tokenRequestEncoding: 'form' },
		};

		const envNames = new Set();
		const envNameFor = (provider) => {
			const base = 'PI_' + provider.toUpperCase().replace(/[^A-Z0-9]+/g, '_').replace(/^_+|_+$/g, '') + '_CREDENTIAL';
			let name = base;
			for (let n = 2; envNames.has(name); n++) name = `${base}_${n}`;
			envNames.add(name);
			return name;
		};

		const plan = [];
		const notices = [];
		for (const provider of Object.keys(auth).sort()) {
			const value = auth[provider];
			if (!value || typeof value !== 'object') continue;
			if (value.type === 'api_key' && typeof value.key === 'string' && value.key.startsWith('!')) {
				// A key that is a command pi runs to obtain it. The command is
				// this sandbox's, and its output is not a credential this flow
				// can hold; the provider is left to be signed in another way.
				notices.push(`${provider}'s key is a command (${value.key}), which cannot be captured. Sign in to ${provider} with the key itself, or an OAuth login.`);
				continue;
			}
			// GitHub Copilot's access token is minted from its GitHub token and
			// lives half an hour; the GitHub token is the credential, and it is
			// what pi refreshes with. So the secret is the refresh field, and
			// the entry carries the sentinel in both fields with no expiry, so
			// pi mints a fresh access token through the proxy on first use —
			// the same shape the opencode image delivers Copilot in.
			const copilot = value.type === 'oauth' && provider === 'github-copilot';
			const material = value.type === 'api_key' ? value.key : value.type === 'oauth' ? (copilot ? value.refresh : value.access) : undefined;
			if (typeof material !== 'string' || !material) continue;
			const envName = envNameFor(provider);
			const sentinel = process.env['PREV_' + envName];
			if (sentinel && material === sentinel && previousEntries.has(envName)) {
				plan.push({ envName, provider, kept: true, type: previousTypes.get(envName) || 'token', entry: previousEntries.get(envName) });
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
				item.type = 'token';
				item.value = { token: value.refresh };
				item.entry = { ...value, access: template, refresh: template, expires: 0 };
			} else {
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
						expires: farFuture,
					};
					const expires = Number(value.expires) || 0;
					if (expires > 0 && expires < farFuture) {
						item.notice = `${provider}'s sign-in expires ${new Date(expires).toISOString().slice(0, 10)}, and Discobox cannot renew it. Configure again after that, or sign in with an API key.`;
					}
				}
			}
			plan.push(item);
		}
		for (const notice of notices) plan.push({ noticeOnly: true, notice });
		fs.writeFileSync(process.env.PI_CONFIGURE_PLAN, JSON.stringify(plan));
		process.stdout.write(String(plan.filter((p) => !p.noticeOnly).length));
	NODE_EOF
}

# plan_labels prints the providers in the plan, comma-separated.
plan_labels() {
	PI_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const plan = JSON.parse(require("fs").readFileSync(process.env.PI_CONFIGURE_PLAN, "utf8"));
		process.stdout.write(plan.filter((p) => !p.noticeOnly).map((p) => p.provider).join(", "));
	'
}

# verify_default_model runs a trivial prompt on the model a sandbox's pi
# starts with — `pi --print` given no --model uses pi's default: the
# `defaultProvider`/`defaultModel` settings, else pi's pick among what the
# signed-in providers offer.
#
# Only that model is checked, and a failure is a warning rather than a gate.
# Whether a request succeeds depends on the model as much as the credential —
# a plan that excludes it, a region it needs opting in to — and pi's model
# list says nothing of either, so checking every provider on a model picked
# for it would report working credentials as broken. What a failure does show
# is that the harness's first session would fail the same way, which is worth
# fixing while pi is one keystroke away.
#
# Stdin is closed: in print mode pi reads whatever is piped to it into the
# prompt, and this sandbox's terminal is not the prompt.
verify_default_model() {
	set +e
	timeout 180 pi --print --no-session 'Reply with exactly: discobox-ok' </dev/null >"$WORK/verify.log" 2>&1
	verify_status=$?
	set -e
	if [ "$verify_status" -eq 0 ] && grep -qi 'discobox-ok' "$WORK/verify.log"; then
		return 0
	fi
	tail -n 5 "$WORK/verify.log" >&2
	return 1
}

# print_notices shows what the plan says about credentials nothing will renew,
# and about ones it could not capture.
print_notices() {
	PI_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const plan = JSON.parse(require("fs").readFileSync(process.env.PI_CONFIGURE_PLAN, "utf8"));
		for (const item of plan) if (item.notice) process.stdout.write(item.notice + "\n");
	'
}

# confirm_launch holds the instructions on screen until the user is ready.
# pi repaints the terminal as it starts, so anything printed right before
# launching it is gone before it can be read. Waiting for Enter means the
# banner is read while it is still the only thing on screen.
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
# the loop can only turn when a person asks it to: an attempt that fails
# without reaching the user — pi refusing to start, say — fails again the
# moment it is retried, and looping on that is a busy loop, not a retry. End of
# input means nobody is there to answer, which fails the configure flow rather
# than spinning.
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
# delivers them, and pi's settings files as the user left them. Secret shapes:
#   - kept:          usePrevious marker, no value.
#   - OAuth renewed: the access token plus refresh material, type oauth.
#   - anything else: a plain token.
write_output() {
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	PI_CONFIGURE_PLAN="$WORK/plan.json" \
		PI_CONFIGURE_HOME="$HOME" \
		PI_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		PI_CONFIGURE_SETTINGS_PATHS="$SETTINGS_PATHS" \
		PI_CONFIGURE_SETTINGS_PATH="$SETTINGS_PATH" \
		PI_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const path = require('path');
		const plan = JSON.parse(fs.readFileSync(process.env.PI_CONFIGURE_PLAN, 'utf8')).filter((item) => !item.noticeOnly);
		const secrets = plan.map((item) => {
			const secret = { envName: item.envName, name: `pi: ${item.provider}`, type: item.type };
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
		// Settings are returned literally, as pi wrote them: they are the
		// user's, and a custom provider in models.json is theirs to configure.
		for (const rel of process.env.PI_CONFIGURE_SETTINGS_PATHS.split(' ').filter(Boolean)) {
			try {
				files.push({ path: rel, content: fs.readFileSync(path.join(process.env.PI_CONFIGURE_HOME, rel), 'utf8') });
			} catch (err) {
				// Not written in this session or any before it; nothing to keep.
			}
		}
		// Discobox's settings, as they arrived — a judge model someone set by
		// editing the file is theirs.
		const settingsPath = process.env.PI_CONFIGURE_SETTINGS_PATH;
		let settings = {};
		try {
			settings = JSON.parse(fs.readFileSync(path.join(process.env.PI_CONFIGURE_HOME, settingsPath), 'utf8')) || {};
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
	printf '%s\n' "${C_BOLD}${C_WARN}=======================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up pi — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}=======================================================${C_RESET}"
	echo
	echo "Discobox is about to start pi so you can set it up. This is a"
	echo "throwaway setup sandbox: it exists only to capture your providers and"
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
	printf '%s\n' "  1. ${C_BOLD}${C_CMD}/login${C_RESET}    Sign in to each provider this harness should use —"
	echo "              as many as you like. This sandbox has no browser: a"
	echo "              sign-in prints a link to open on your own machine."
	echo "              ChatGPT's and Anthropic's sign-ins are forwarded back"
	echo "              in here; if Discobox warned their port is taken, use"
	echo "              ChatGPT's device code, or paste Anthropic's code when"
	echo "              pi asks for it. Other browser sign-ins cannot reach"
	echo "              back in here: paste the code, or use an API key."
	if [ -n "$SEEDED_LABELS" ]; then
		echo "              (Optional now: providers you already signed in to are kept.)"
	fi
	printf '%s\n' "  2. ${C_BOLD}${C_CMD}/quit${C_RESET}     Leave pi when you're done. Setup only finishes"
	echo "              once you exit — staying in blocks it."
	echo
	echo "Worth doing while you're in there:"
	echo
	printf '%s\n' "  ${C_CMD}/model${C_RESET}      Pick the model this harness starts with"
	printf '%s\n' "  ${C_CMD}/settings${C_RESET}   Thinking level, theme"
	echo
	confirm_launch

	# pi needs a real TTY for its interactive UI; run it under script.
	set +e
	script -q -e -c 'pi' /dev/null
	pi_status=$?
	set -e
	# pi can leave its UI's contents behind when it exits, and everything
	# printed next would be drawn over them. Only after a clean exit: when pi
	# fails, what it printed is the explanation, and clearing would erase it.
	if [ "$pi_status" -eq 0 ] && [ -t 1 ]; then
		printf '\033[?1049l\033[H\033[2J'
	fi
	echo

	count=$(classify_credentials)

	if [ "$count" -eq 0 ]; then
		if [ "$pi_status" -ne 0 ]; then
			# Not "the user signed in to nothing": pi refused to run at all,
			# and saying so points at the message it printed just above.
			printf '%s\n' "${C_ERR}${C_BOLD}pi exited with status $pi_status without a provider signed in.${C_RESET}" >&2
		else
			printf '%s\n' "${C_ERR}${C_BOLD}No provider is signed in.${C_RESET}" >&2
			echo "pi needs a provider to answer at all, so a sandbox on this harness will"
			echo "have nothing to run on until one is signed in."
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
	echo "Your providers are still signed in. Start pi again to pick another"
	echo "model with /model, or sign in to the provider again with /login."
	if confirm_retry "saves everything as it is"; then
		continue
	fi
	break
done

print_notices
write_output

echo
echo "pi configuration complete."
