#!/bin/sh
# Runs as the configure sandbox's primary terminal (see "Configure flows"
# in harness/DESIGN.md). Launches omp interactively, lets the user sign in to
# whichever providers they want and set omp up however they normally would,
# then reads back what omp itself stored to capture the result. Writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# omp keeps every credential in SQLite (~/.omp/agent/agent.db), and moves them
# with its own `omp auth export` and `omp auth import`: a versioned JSON
# document of {provider, credential} entries (ADR 26-10-14-718 §2). So this
# script launches a bare `omp` and afterwards exports what it stored:
#   - Each credential becomes one secret, OMP_<PROVIDER>_CREDENTIAL.
#   - An API key is a `token` secret.
#   - An OAuth login the control plane knows how to renew (see oauthRefresh
#     below) is an `oauth` secret with its refresh material.
#   - Any other OAuth login is a `token` secret holding the access token, and
#     the script says when it expires, because nothing will renew it.
# The default model is checked with a one-line `omp -p`, and a failure is
# reported rather than refused (see verify_default_model).
#
# No credential is exported as an environment variable. The script returns the
# export document as a templated harness file with each credential's sentinel
# in place of its secret, which the image's launcher imports into omp at every
# launch.
#
# It also returns omp's settings as the user left them (config.yml: the default
# model, theme and the rest), and Discobox's own settings for the harness.
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each one's value is
# available as $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real
# credential on the way out — so the previous document is imported with
# sentinels and the session opens already signed in. A credential still
# holding its sentinel afterwards was not replaced, and is reported back as
# usePrevious rather than as a value.
set -eu

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json

# Home-relative, like every harness file.
CREDENTIALS_PATH=".config/discobox/omp-credentials.json"
OMP_SETTINGS_PATH=".omp/agent/config.yml"
# Discobox's own settings for this harness: the judge model. The image declares
# a baseline, and a configured harness delivers its own copy into this sandbox,
# which is what a reconfigure starts from.
SETTINGS_PATH=".config/discobox/omp-harness.json"
AGENT_DIR="$HOME/.omp/agent"

# The `expires` a delivered OAuth entry carries when the control plane renews
# its token: far enough out that omp never decides the credential is stale and
# tries a refresh itself. That is not a lie about the real token — what the
# entry carries is a sentinel, which genuinely does not go stale, and the
# refresh token it would need never leaves the control plane.
FAR_FUTURE_EXPIRY=4102444800000
REFRESH_PLACEHOLDER="discobox-refresh-happens-in-the-control-plane"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# The egress proxy for omp's Anthropic transport, which reads no other proxy
# variable (proxy-env.sh): without it a sign-in works and every Anthropic
# request then hangs.
if [ -r /etc/profile.d/omp-proxy.sh ]; then
	# shellcheck disable=SC1091 # Installed by the image (proxy-env.sh).
	. /etc/profile.d/omp-proxy.sh
fi

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

# seed_previous_credentials writes, to $WORK/seed.json, the export document a
# previous run captured with each PREV_ sentinel in place of its template
# action, so the session below opens already signed in.
#
# This is what lets a reconfigure be about anything else. Reconfigure is usually
# about settings — a model, a theme, one more provider — and a flow that opened
# signed out of everything would make changing a setting cost every sign-in.
#
# What is imported is the PREV_ sentinel, not the credential: the proxy swaps it
# on outbound requests, so the session is genuinely signed in while this
# sandbox never holds the real value. The sentinel is also how the change check
# works afterwards — it is a value we know, so finding it still in place means
# the credential was not replaced (see classify_credentials).
#
# A credential whose sentinel is not set — its secret was deleted, say — is
# left out, and opens signed out, which is the one case a sign-in is
# unavoidable.
seed_previous_credentials() {
	OMP_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		OMP_CONFIGURE_CREDENTIALS_PATH="$CREDENTIALS_PATH" \
		OMP_CONFIGURE_SEED="$WORK/seed.json" \
		OMP_CONFIGURE_SEEDED="$WORK/seeded.txt" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.OMP_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const kept = (previous.files || []).find((f) => f && f.path === process.env.OMP_CONFIGURE_CREDENTIALS_PATH);
		let template = {};
		try {
			template = JSON.parse(kept && typeof kept.content === 'string' ? kept.content : '{}') || {};
		} catch (err) {
			template = {};
		}
		const action = /\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;
		const credentials = [];
		for (const entry of Array.isArray(template.credentials) ? template.credentials : []) {
			const text = JSON.stringify(entry);
			const names = [...text.matchAll(action)].map((m) => m[1]);
			if (names.length === 0 || names.some((name) => !process.env['PREV_' + name])) continue;
			credentials.push(JSON.parse(text.replace(action, (_, name) => process.env['PREV_' + name])));
		}
		if (credentials.length > 0) {
			fs.writeFileSync(process.env.OMP_CONFIGURE_SEED, JSON.stringify({ version: 1, credentials }), { mode: 0o600 });
		}
		fs.writeFileSync(process.env.OMP_CONFIGURE_SEEDED, [...new Set(credentials.map((c) => c.provider))].join(', '));
	NODE_EOF
	SEEDED_LABELS=$(cat "$WORK/seeded.txt")
}

# reset_stored_credentials leaves omp holding exactly the seeded credentials:
# its store is removed and the seed imported into a fresh one, so a retry's
# capture cannot mistake a sign-in from an earlier attempt in this same sandbox
# for a fresh one. The store holds nothing else worth keeping here — usage
# counters and caches omp rebuilds.
#
# A configured harness also delivers its credentials file into this sandbox,
# but rendered against secrets this sandbox does not have (they arrive
# PREV_-prefixed), so it describes credentials with nothing behind them and is
# never imported; the seed is.
reset_stored_credentials() {
	rm -f "$AGENT_DIR/agent.db" "$AGENT_DIR/agent.db-wal" "$AGENT_DIR/agent.db-shm"
	if [ -f "$WORK/seed.json" ]; then
		omp auth import "$WORK/seed.json" >/dev/null 2>&1 </dev/null || {
			echo "omp could not import the credentials you configured before; sign in again." >&2
			rm -f "$WORK/seed.json"
			SEEDED_LABELS=""
		}
	fi
}

# classify_credentials exports what omp stored and turns it into
# $WORK/plan.json: one entry per credential, with the secret to store (or the
# note that the previous one is kept) and the export entry that delivers it.
# Prints how many there are.
#
# A credential whose secret material is still the PREV_ sentinel seeded for its
# name was not replaced, and is kept with the entry the last run returned —
# which carries whatever the real sign-in recorded, and which the seed was
# rendered from. Anything else is a new credential and is captured.
classify_credentials() {
	omp auth export >"$WORK/export.json" 2>/dev/null </dev/null || echo '{"version":1,"credentials":[]}' >"$WORK/export.json"
	OMP_CONFIGURE_EXPORT="$WORK/export.json" \
		OMP_CONFIGURE_PLAN="$WORK/plan.json" \
		OMP_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		OMP_CONFIGURE_CREDENTIALS_PATH="$CREDENTIALS_PATH" \
		OMP_CONFIGURE_FAR_FUTURE_EXPIRY="$FAR_FUTURE_EXPIRY" \
		OMP_CONFIGURE_REFRESH_PLACEHOLDER="$REFRESH_PLACEHOLDER" node <<-'NODE_EOF'
		const fs = require('fs');
		let exported = {};
		try {
			exported = JSON.parse(fs.readFileSync(process.env.OMP_CONFIGURE_EXPORT, 'utf8')) || {};
		} catch (err) {
			exported = {};
		}
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.OMP_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const farFuture = Number(process.env.OMP_CONFIGURE_FAR_FUTURE_EXPIRY);
		const placeholder = process.env.OMP_CONFIGURE_REFRESH_PLACEHOLDER;

		// The previous run's entries and secret types, by the env name each
		// entry's template action names.
		const action = /\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/;
		const previousEntries = new Map();
		const kept = (previous.files || []).find((f) => f && f.path === process.env.OMP_CONFIGURE_CREDENTIALS_PATH);
		try {
			const doc = JSON.parse(kept ? kept.content : '{}') || {};
			for (const entry of Array.isArray(doc.credentials) ? doc.credentials : []) {
				const match = JSON.stringify(entry).match(action);
				if (match) previousEntries.set(match[1], entry);
			}
		} catch (err) {
			// A previous file that does not parse keeps nothing.
		}
		const previousTypes = new Map((previous.secrets || []).map((s) => [s && s.envName, s && s.type]));

		// oauthRefresh: the providers whose OAuth login the control plane can
		// renew, and how. Each is a property of that provider's public OAuth
		// client — its token endpoint, client id, and request encoding — not of
		// any one sign-in, so they are fixed here rather than read from the
		// export. omp signs in to these with the same clients the claude-code and
		// codex images (and opencode's xAI) are refreshed for. A provider not
		// listed is delivered as a plain token.
		const oauthRefresh = {
			anthropic: { tokenUrl: 'https://console.anthropic.com/v1/oauth/token', clientId: '9d1c250a-e61b-44d9-88ed-5944d1962f5e' },
			'openai-codex': { tokenUrl: 'https://auth.openai.com/oauth/token', clientId: 'app_EMoamEEZ73f0CkXaXp7hrann' },
			// xAI's token endpoint takes the refresh request form-encoded.
			'xai-oauth': { tokenUrl: 'https://auth.x.ai/oauth2/token', clientId: 'b1a00492-073a-47ea-816f-4c329264a828', tokenRequestEncoding: 'form' },
		};

		// Which secret each credential is. omp keeps several accounts of one
		// provider side by side, and exports them in whatever order its store
		// holds them, which a sign-in replacing one can change — so a position
		// in the export names nothing. Three passes, each taking only names the
		// ones before it left:
		//   1. A credential still holding the PREV_ sentinel of a previous
		//      secret is that secret, kept.
		//   2. A new credential for an account a previous secret held — the
		//      same provider, accountId and email — replaces that secret in
		//      place.
		//   3. Anything else takes the provider's lowest free name: the first
		//      account OMP_<PROVIDER>_CREDENTIAL, the next _2.
		const baseName = (provider) => 'OMP_' + provider.toUpperCase().replace(/[^A-Z0-9]+/g, '_').replace(/^_+|_+$/g, '') + '_CREDENTIAL';
		const identity = (provider, credential) =>
			credential && (credential.accountId || credential.email) ? JSON.stringify([provider, credential.accountId || '', credential.email || '']) : '';
		const sentinels = new Map();
		for (const name of previousEntries.keys()) {
			const sentinel = process.env['PREV_' + name];
			if (sentinel) sentinels.set(sentinel, name);
		}
		const credentials = [];
		for (const item of Array.isArray(exported.credentials) ? exported.credentials : []) {
			const provider = item && item.provider;
			const value = item && item.credential;
			if (typeof provider !== 'string' || !provider || !value || typeof value !== 'object') continue;
			const material = value.type === 'api_key' ? value.key : value.type === 'oauth' ? value.access : undefined;
			if (typeof material !== 'string' || !material) continue;
			credentials.push({ provider, value, material });
		}
		const taken = new Set();
		for (const c of credentials) {
			const name = sentinels.get(c.material);
			if (name && !taken.has(name)) {
				c.envName = name;
				c.kept = true;
				taken.add(name);
			}
		}
		for (const c of credentials) {
			// A sentinel no secret claimed is a copy of one already kept, and
			// a sentinel is never captured as a credential's value.
			if (c.envName || sentinels.has(c.material)) continue;
			const id = identity(c.provider, c.value);
			if (!id) continue;
			for (const [name, entry] of previousEntries) {
				if (!taken.has(name) && identity(entry.provider, entry.credential) === id) {
					c.envName = name;
					taken.add(name);
					break;
				}
			}
		}
		for (const c of credentials) {
			if (c.envName || sentinels.has(c.material)) continue;
			const base = baseName(c.provider);
			let name = base;
			for (let n = 2; taken.has(name); n++) name = `${base}_${n}`;
			c.envName = name;
			taken.add(name);
		}

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

		const plan = [];
		for (const { provider, value, envName, kept } of credentials) {
			if (!envName) continue;
			const label = value.email ? `omp: ${provider} (${value.email})` : `omp: ${provider}`;
			if (kept) {
				const entry = previousEntries.get(envName);
				const type = previousTypes.get(envName) || 'token';
				plan.push({ envName, provider, label, kept: true, type, entry, notice: type === 'token' ? notice(provider, entry.credential) : undefined });
				continue;
			}
			// Dotted field access, never a quoted map key: this content is JSON,
			// so a quote inside a template action is escaped into the file itself
			// and Go's template parser rejects it — which breaks every sandbox
			// launch while configure still reports success. An env name is
			// always a valid template field name, so there is nothing to quote.
			const template = `{{ .secrets.${envName} }}`;
			const planned = { envName, provider, label, kept: false };
			if (value.type === 'api_key') {
				planned.type = 'token';
				planned.value = { token: value.key };
				planned.entry = { provider, credential: { ...value, key: template } };
			} else {
				const spec = oauthRefresh[provider];
				if (spec && value.refresh && value.refresh !== value.access) {
					planned.type = 'oauth';
					planned.value = { token: value.access, refreshToken: value.refresh, tokenUrl: spec.tokenUrl, clientId: spec.clientId };
					if (spec.tokenRequestEncoding) planned.value.tokenRequestEncoding = spec.tokenRequestEncoding;
					if (Number(value.expires) > 0) planned.value.accessTokenExpiresAt = Number(value.expires);
					// The refresh token stays in the control plane, so a refresh
					// from inside a sandbox could not succeed — and does not need
					// to, since a sentinel does not go stale.
					planned.entry = { provider, credential: { ...value, access: template, refresh: placeholder, expires: farFuture } };
				} else {
					planned.type = 'token';
					planned.value = { token: value.access };
					// A provider that authenticates with the refresh field itself
					// (GitHub Copilot stores its token in both) gets the sentinel
					// there too; a real refresh token becomes the placeholder.
					planned.entry = {
						provider,
						credential: {
							...value,
							access: template,
							refresh: value.refresh === value.access ? template : placeholder,
						},
					};
					planned.notice = notice(provider, value);
				}
			}
			plan.push(planned);
		}
		fs.writeFileSync(process.env.OMP_CONFIGURE_PLAN, JSON.stringify(plan));
		process.stdout.write(String(plan.length));
	NODE_EOF
}

# plan_labels prints the providers in the plan, comma-separated.
plan_labels() {
	OMP_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const plan = JSON.parse(require("fs").readFileSync(process.env.OMP_CONFIGURE_PLAN, "utf8"));
		process.stdout.write([...new Set(plan.map((p) => p.provider))].join(", "));
	'
}

# verify_default_model runs a trivial, tool-free prompt on the model a
# sandbox's omp starts with — `omp -p` given no --model uses omp's default: the
# default model role, else omp's pick among the signed-in providers. It runs
# from an empty directory, so no project's settings choose instead.
#
# Only that model is checked, and a failure is a warning rather than a gate.
# Whether a request succeeds depends on the model as much as the credential —
# a plan that excludes it, a region it needs opting in to — and omp's model list
# says nothing of either, so checking every credential on a model picked for it
# would report working credentials as broken. What a failure does show is that
# the harness's first session would fail the same way, which is worth fixing
# while omp is one keystroke away.
verify_default_model() {
	mkdir -p "$WORK/verify"
	set +e
	(cd "$WORK/verify" && timeout 180 omp -p --no-tools --no-session --no-title -- 'Reply with exactly: discobox-ok' \
		</dev/null >"$WORK/verify.log" 2>&1)
	verify_status=$?
	set -e
	if [ "$verify_status" -eq 0 ] && grep -qi 'discobox-ok' "$WORK/verify.log"; then
		return 0
	fi
	tail -n 5 "$WORK/verify.log" >&2
	return 1
}

# print_notices shows what the plan says about credentials nothing will renew.
print_notices() {
	OMP_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const plan = JSON.parse(require("fs").readFileSync(process.env.OMP_CONFIGURE_PLAN, "utf8"));
		for (const item of plan) if (item.notice) process.stdout.write(item.notice + "\n");
	'
}

# confirm_launch holds the instructions on screen until the user is ready.
# omp draws a full-screen TUI, so anything printed right before launching it is
# gone before it can be read. Waiting for Enter means the banner is read while
# it is still the only thing on screen.
#
# End of input is not "yes": nobody is there, and launching an interactive TUI
# at nobody wedges the configure flow rather than failing it.
confirm_launch() {
	printf '%s' "${C_BOLD}Press Enter to start omp.${C_RESET} "
	if ! read -r _launch_ack; then
		echo >&2
		echo "No input; aborting without configuring omp." >&2
		exit 1
	fi
	echo
}

# confirm_retry asks whether to launch omp again, and says what answering no
# does, since here it is not always "abort". Every retry goes through here so
# the loop can only turn when a person asks it to: an attempt that fails without
# reaching the user — omp refusing to start, say — fails again the moment it is
# retried, and looping on that is a busy loop, not a retry. End of input means
# nobody is there to answer, which fails the configure flow rather than
# spinning.
confirm_retry() {
	printf 'Start omp again? Answering n %s. [Y/n] ' "$1"
	if ! read -r retry_choice; then
		echo >&2
		echo "No input; aborting without configuring omp." >&2
		exit 1
	fi
	echo
	case "${retry_choice:-y}" in
	[nN]*) return 1 ;;
	esac
	return 0
}

# write_output records the result: one secret per credential, the export
# document that delivers them, omp's settings as the user left them, and
# Discobox's settings for the harness. Secret shapes:
#   - kept:          usePrevious marker, no value.
#   - OAuth renewed: the access token plus refresh material, type oauth.
#   - anything else: a plain token.
write_output() {
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	OMP_CONFIGURE_PLAN="$WORK/plan.json" \
		OMP_CONFIGURE_HOME="$HOME" \
		OMP_CONFIGURE_CREDENTIALS_PATH="$CREDENTIALS_PATH" \
		OMP_CONFIGURE_OMP_SETTINGS_PATH="$OMP_SETTINGS_PATH" \
		OMP_CONFIGURE_SETTINGS_PATH="$SETTINGS_PATH" \
		OMP_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const path = require('path');
		const home = process.env.OMP_CONFIGURE_HOME;
		const plan = JSON.parse(fs.readFileSync(process.env.OMP_CONFIGURE_PLAN, 'utf8'));
		const secrets = plan.map((item) => {
			const secret = { envName: item.envName, name: item.label, type: item.type };
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
			files.push({
				path: process.env.OMP_CONFIGURE_CREDENTIALS_PATH,
				template: true,
				content: JSON.stringify({ version: 1, credentials: plan.map((item) => item.entry) }, null, 2) + '\n',
			});
		}
		// omp's settings are returned literally, as omp wrote them: they are the
		// user's.
		try {
			const rel = process.env.OMP_CONFIGURE_OMP_SETTINGS_PATH;
			files.push({ path: rel, content: fs.readFileSync(path.join(home, rel), 'utf8') });
		} catch (err) {
			// Nothing was set in this session or any before it; nothing to keep.
		}
		// Discobox's settings, as they arrived — a judge model someone set by
		// editing the file is theirs.
		const settingsPath = process.env.OMP_CONFIGURE_SETTINGS_PATH;
		let settings = {};
		try {
			settings = JSON.parse(fs.readFileSync(path.join(home, settingsPath), 'utf8')) || {};
		} catch (err) {
			settings = {};
		}
		if (typeof settings.judgeModel !== 'string') settings.judgeModel = '';
		files.push({ path: settingsPath, content: JSON.stringify(settings, null, 2) + '\n' });
		fs.writeFileSync(process.env.OMP_CONFIGURE_OUTPUT, JSON.stringify({ files, secrets }));
	NODE_EOF
}

SEEDED_LABELS=""
seed_previous_credentials

while :; do
	reset_stored_credentials

	printf '%s\n' "${C_BOLD}${C_WARN}========================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up omp — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}========================================================${C_RESET}"
	echo
	echo "Discobox is about to start omp so you can set it up. This is a"
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
	echo "             your browser lands on and paste it into omp."
	if [ -n "$SEEDED_LABELS" ]; then
		echo "             (Optional now: providers you already signed in to are kept.)"
	fi
	printf '%s\n' "  2. ${C_BOLD}${C_CMD}/exit${C_RESET}    Leave omp when you're done. Setup only finishes"
	echo "             once you exit — staying in blocks it."
	echo
	echo "Worth doing while you're in there:"
	echo
	printf '%s\n' "  ${C_CMD}/model${C_RESET}     Pick the default model this harness starts with"
	printf '%s\n' "  ${C_CMD}/settings${C_RESET}  Theme, prompt layout, and the rest"
	echo
	confirm_launch

	# omp needs a real TTY for its interactive UI; run it under script. The
	# image's settings overlay is left out here, so a first configure gets
	# omp's own setup wizard — the sign-in and default model this is for.
	set +e
	env -u PI_CONFIG_FILES script -q -e -c 'omp --allow-home' /dev/null
	omp_status=$?
	set -e
	# omp can leave its full-screen UI's contents behind when it exits, and
	# everything printed next would be drawn over them.
	# Only after a clean exit: when omp fails, what it printed is the
	# explanation, and clearing would erase it.
	if [ "$omp_status" -eq 0 ] && [ -t 1 ]; then
		printf '\033[?1049l\033[H\033[2J'
	fi
	echo

	count=$(classify_credentials)

	if [ "$count" -eq 0 ]; then
		if [ "$omp_status" -ne 0 ]; then
			# Not "the user signed in to nothing": omp refused to run at all,
			# and saying so points at the message it printed just above.
			printf '%s\n' "${C_ERR}${C_BOLD}omp exited with status $omp_status without signing in to a provider.${C_RESET}" >&2
		else
			printf '%s\n' "${C_ERR}${C_BOLD}No provider is signed in.${C_RESET}" >&2
			echo "omp still runs without one — with a local model server, or a provider"
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
	printf '%s\n' "${C_ERR}${C_BOLD}omp's default model did not answer.${C_RESET}" >&2
	echo "Your sign-ins are kept. Start omp again to pick another model with"
	echo "/model, or sign in again with /login."
	if confirm_retry "saves everything as it is"; then
		# The sign-ins this round made are the next round's starting point,
		# so starting again does not cost them.
		cp "$WORK/export.json" "$WORK/seed.json"
		SEEDED_LABELS=$(plan_labels)
		continue
	fi
	break
done

print_notices
write_output

echo
echo "omp configuration complete."
