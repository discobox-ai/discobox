#!/bin/sh
# Runs as the configure sandbox's primary terminal (see "Configure flows"
# in harness/DESIGN.md). Launches opencode interactively, lets the user connect
# whichever providers they want and configure it however they normally would,
# then reads what opencode itself stored to capture the result. Writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# opencode 2 keeps every provider credential as a row in its own database, and
# `/connect` already offers every provider's own sign-in — API keys, and the
# OAuth sign-ins some providers have (ChatGPT, GitHub Copilot, SuperGrok, ...).
# So this script launches a bare `opencode` and afterwards reads the store with
# `opencode auth export`, opencode's own interchange format (ADR 26-10-15-848):
#   - Each provider becomes one secret, OPENCODE_<PROVIDER>_CREDENTIAL.
#   - An API key is a `token` secret.
#   - An OAuth sign-in the control plane knows how to renew (see oauthRefresh
#     below) is an `oauth` secret with its refresh material.
#   - Any other OAuth sign-in is a `token` secret holding the access token, and
#     the script says when it expires, because nothing will renew it.
#   - A credential opencode fetches from outside itself (an `external` one, such
#     as a cloud CLI's login) holds no secret, and is not captured.
# The default model is checked with a one-line `opencode run`, and a failure
# is reported rather than refused (see verify_default_model).
#
# No credential is exported as an environment variable. The script returns the
# export as a templated harness file with each provider's sentinel in place of
# its secret, which the launcher imports into a sandbox's store
# (opencode-credentials sync).
#
# It asks whether opencode may search the web, and records the answer in
# Discobox's settings for this harness.
#
# It also returns opencode's settings as the user left them — the global
# opencode.json(c) and config.json, cli.json (theme, keybinds), and the model
# picked with /models — so they become the harness's defaults.
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each one's value is
# available as $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real
# credential on the way out — so the previous credentials are imported back
# with sentinels and the session opens already connected. A credential still
# holding its sentinel afterwards was not replaced, and is reported back as
# usePrevious rather than as a value. A configuration made under ADR 0127
# returned opencode 1's auth.json instead, which is read the way the launcher
# reads it.
set -eu
HARNESS_CAPTURE_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
export HARNESS_CAPTURE_DIR

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json
CREDENTIALS_HELPER=/usr/local/libexec/discobox/opencode-credentials

# Home-relative, like every harness file.
CREDENTIALS_PATH=".config/discobox/opencode-credentials.json"
LEGACY_AUTH_PATH=".local/share/opencode/auth.json"
SETTINGS_PATHS=".config/opencode/opencode.json .config/opencode/opencode.jsonc .config/opencode/config.json .config/opencode/cli.json"
MODEL_PREFERENCE_PATH=".local/state/opencode/model.json"
# Discobox's own settings for this harness: web search. The
# image declares a baseline, and a configured harness delivers its own copy
# into this sandbox, which is what a reconfigure starts from.
SETTINGS_PATH=".config/discobox/opencode-harness.json"

# The `expires` a delivered OAuth entry carries when the control plane renews
# its token: far enough out that opencode never decides the credential is stale
# and tries a refresh itself. That is not a lie about the real token — what the
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

# previous_entries writes $WORK/previous.json: the credential entries a
# previous run returned, as import entries with their template actions still
# in them. They are read the way the launcher reads what a sandbox was
# delivered — so a configuration made under ADR 0127, which returned opencode
# 1's auth.json instead, is read as one too.
previous_entries() {
	OPENCODE_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		OPENCODE_CONFIGURE_PATHS="$CREDENTIALS_PATH $LEGACY_AUTH_PATH" \
		OPENCODE_CONFIGURE_PREVIOUS_HOME="$WORK/previous-home" node <<-'NODE_EOF'
		const fs = require('fs');
		const path = require('path');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.OPENCODE_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		for (const rel of process.env.OPENCODE_CONFIGURE_PATHS.split(' ')) {
			const file = (previous.files || []).find((f) => f && f.path === rel && typeof f.content === 'string');
			if (!file) continue;
			const target = path.join(process.env.OPENCODE_CONFIGURE_PREVIOUS_HOME, rel);
			fs.mkdirSync(path.dirname(target), { recursive: true });
			fs.writeFileSync(target, file.content);
		}
	NODE_EOF
	"$CREDENTIALS_HELPER" delivered "$WORK/previous-home" >"$WORK/previous.json" 2>/dev/null ||
		echo '[]' >"$WORK/previous.json"
}

# seed_previous_credentials imports the credentials a previous run captured
# into opencode's store, so the session below opens already connected.
#
# This is what lets a reconfigure be about anything else. Reconfigure is usually
# about settings — a model, a theme, one more provider — and a flow that opened
# signed out of everything would make changing a setting cost every sign-in.
#
# What is imported is the PREV_ sentinel, not the credential: the proxy swaps it
# on outbound requests, so the session is genuinely connected while this sandbox
# never holds the real value. The sentinel is also how the change check works
# afterwards — it is a value we know, so finding it still in place means the
# credential was not replaced (see classify_credentials).
#
# A provider whose sentinel is not set — its secret was deleted, say — is left
# out, and opens signed out, which is the one case a sign-in is unavoidable.
#
# A configured harness delivers its files into this sandbox like any other,
# but their templates render against secrets this sandbox does not have (they
# arrive PREV_-prefixed), so what lands describes credentials with nothing
# behind them. An opencode 1 auth.json among them is removed; opencode 2 does
# not read it, and nothing should mistake it for a sign-in.
seed_previous_credentials() {
	rm -f "$HOME/$LEGACY_AUTH_PATH"
	previous_entries
	OPENCODE_CONFIGURE_PREVIOUS_ENTRIES="$WORK/previous.json" \
		OPENCODE_CONFIGURE_SEED="$WORK/seed.json" \
		OPENCODE_CONFIGURE_SEEDED="$WORK/seeded.txt" node <<-'NODE_EOF'
		const fs = require('fs');
		const entries = JSON.parse(fs.readFileSync(process.env.OPENCODE_CONFIGURE_PREVIOUS_ENTRIES, 'utf8'));
		const action = /\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;
		const seeded = [];
		for (const entry of entries) {
			const text = JSON.stringify(entry);
			const names = [...text.matchAll(action)].map((m) => m[1]);
			if (names.length === 0 || names.some((name) => !process.env['PREV_' + name])) continue;
			seeded.push(JSON.parse(text.replace(action, (_, name) => process.env['PREV_' + name])));
		}
		fs.writeFileSync(process.env.OPENCODE_CONFIGURE_SEED, JSON.stringify(seeded));
		fs.writeFileSync(process.env.OPENCODE_CONFIGURE_SEEDED, seeded.map((e) => e.integrationID).join(', '));
	NODE_EOF
	SEEDED_LABELS=$(cat "$WORK/seeded.txt")
	if [ -n "$SEEDED_LABELS" ] && ! opencode auth import --standalone <"$WORK/seed.json" >/dev/null 2>&1; then
		# Opening signed out is still a configure flow; it only costs the
		# sign-ins again.
		printf '%s\n' "Could not restore the providers already connected ($SEEDED_LABELS); connect them again." >&2
		SEEDED_LABELS=""
	fi
}

# classify_credentials turns opencode's store into $WORK/plan.json: one entry
# per provider, with the secret to store (or the note that the previous one is
# kept) and the import entry that delivers it. Prints how many there are.
#
# A credential whose secret material is still the PREV_ sentinel seeded for its
# name was not replaced, and is kept with the entry the last run returned —
# which carries whatever the real sign-in recorded, and which the seed was
# rendered from. Anything else is a new credential and is captured.
classify_credentials() {
	if ! opencode auth export --standalone </dev/null >"$WORK/export.json" 2>/dev/null; then
		echo '[]' >"$WORK/export.json"
	fi
	OPENCODE_CONFIGURE_EXPORT="$WORK/export.json" \
		OPENCODE_CONFIGURE_PLAN="$WORK/plan.json" \
		OPENCODE_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		OPENCODE_CONFIGURE_PREVIOUS_ENTRIES="$WORK/previous.json" \
		OPENCODE_CONFIGURE_FAR_FUTURE_EXPIRY="$FAR_FUTURE_EXPIRY" \
		OPENCODE_CONFIGURE_REFRESH_PLACEHOLDER="$REFRESH_PLACEHOLDER" node <<-'NODE_EOF'
		const fs = require('fs');
		const path = require('path');
		const dir = process.env.HARNESS_CAPTURE_DIR;
		const { normalize } = require(path.join(dir, 'opencode-normalize.cjs'));
		const definition = require(path.join(dir, 'opencode-image.json')).harness.saveBack;
		const read = (p, fallback) => { try { return JSON.parse(fs.readFileSync(p, 'utf8')); } catch { return fallback; } };
		const sentinels = Object.fromEntries(Object.entries(process.env).filter(([k]) => k.startsWith('PREV_')).map(([k,v]) => [k.slice(5),v]));
		const previous = read(process.env.OPENCODE_CONFIGURE_PREVIOUS, {});
		previous.files = (previous.files || []).filter(f => f.path !== '.config/discobox/opencode-credentials.json');
		previous.files.push({ path: '.config/discobox/opencode-credentials.json', content: JSON.stringify(read(process.env.OPENCODE_CONFIGURE_PREVIOUS_ENTRIES, [])) });
		const result = normalize(read(process.env.OPENCODE_CONFIGURE_EXPORT, []), previous, sentinels, definition);
		fs.writeFileSync(process.env.OPENCODE_CONFIGURE_PLAN, JSON.stringify(result.plan));
		process.stdout.write(String(result.plan.length));
	NODE_EOF
}

# plan_labels prints the providers in the plan, comma-separated.
plan_labels() {
	OPENCODE_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const plan = JSON.parse(require("fs").readFileSync(process.env.OPENCODE_CONFIGURE_PLAN, "utf8"));
		process.stdout.write(plan.map((p) => p.provider).join(", "));
	'
}

# verify_default_model runs a trivial prompt on the model a sandbox's opencode
# starts with: the `model` setting, else the last model picked with /models
# (opencode-model), else opencode's own pick among what the connected providers
# offer. `opencode run` given no --model skips the /models pick, so the model
# is named.
#
# Only that model is checked, and a failure is a warning rather than a gate.
# Whether a request succeeds depends on the model as much as the credential —
# a plan that excludes it, a region it needs opting in to — and opencode's
# model list says nothing of either, so checking every provider on a model
# picked for it would report working credentials as broken. What a failure
# does show is that the harness's first session would fail the same way, which
# is worth fixing while opencode is one keystroke away.
verify_default_model() {
	model=$(/usr/local/libexec/discobox/opencode-model 2>/dev/null) || model=""
	set +e
	if [ -n "$model" ]; then
		timeout 180 opencode run --standalone "--model=$model" 'Reply with exactly: discobox-ok' </dev/null >"$WORK/verify.log" 2>&1
	else
		timeout 180 opencode run --standalone 'Reply with exactly: discobox-ok' </dev/null >"$WORK/verify.log" 2>&1
	fi
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
	OPENCODE_CONFIGURE_PLAN="$WORK/plan.json" node -e '
		const plan = JSON.parse(require("fs").readFileSync(process.env.OPENCODE_CONFIGURE_PLAN, "utf8"));
		for (const item of plan) if (item.notice) process.stdout.write(item.notice + "\n");
	'
}

# confirm_launch holds the instructions on screen until the user is ready.
# opencode draws a full-screen TUI, so anything printed right before launching
# it is gone before it can be read. Waiting for Enter means the banner is read
# while it is still the only thing on screen.
#
# End of input is not "yes": nobody is there, and launching an interactive TUI
# at nobody wedges the configure flow rather than failing it.
confirm_launch() {
	printf '%s' "${C_BOLD}Press Enter to start opencode.${C_RESET} "
	if ! read -r _launch_ack; then
		echo >&2
		echo "No input; aborting without configuring opencode." >&2
		exit 1
	fi
	echo
}

# confirm_retry asks whether to launch opencode again, and says what answering
# no does, since here it is not always "abort". Every retry goes through here
# so the loop can only turn when a person asks it to: an attempt that fails
# without reaching the user — opencode refusing to start, say — fails again the
# moment it is retried, and looping on that is a busy loop, not a retry. End of
# input means nobody is there to answer, which fails the configure flow rather
# than spinning.
confirm_retry() {
	printf 'Start opencode again? Answering n %s. [Y/n] ' "$1"
	if ! read -r retry_choice; then
		echo >&2
		echo "No input; aborting without configuring opencode." >&2
		exit 1
	fi
	echo
	case "${retry_choice:-y}" in
	[nN]*) return 1 ;;
	esac
	return 0
}

# ask_web_search sets WEB_SEARCH to true or false. opencode 2 searches the web
# with every provider by default, with a keyless search service, so the
# question is whether to turn that off. The previous answer is the default,
# and yes is the default when there is none.
#
# End of input is not an answer: nobody is there, and saving a choice nobody
# made would be quietly deciding for them.
ask_web_search() {
	previous=$(jq -r '.webSearch | if . == false then "n" else "y" end' "$HOME/$SETTINGS_PATH" 2>/dev/null || echo y)
	if [ "$previous" = n ]; then
		hint="[y/N]"
	else
		hint="[Y/n]"
	fi
	echo
	echo "opencode can search the web with any provider, through a search service that"
	echo "needs no key. Answering n takes the web search tool away from every model."
	printf 'Let opencode search the web? %s ' "$hint"
	if ! read -r web_search_choice; then
		echo >&2
		echo "No input; aborting without configuring opencode." >&2
		exit 1
	fi
	case "${web_search_choice:-$previous}" in
	[nN]*) WEB_SEARCH=false ;;
	*) WEB_SEARCH=true ;;
	esac
}

# write_output records the result: one secret per provider, the import entries
# that deliver them, and opencode's settings files as the user left them. Secret
# shapes:
#   - kept:          usePrevious marker, no value.
#   - OAuth renewed: the access token plus refresh material, type oauth.
#   - anything else: a plain token.
write_output() {
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	jq '[.[].entry]' "$WORK/plan.json" | "$CREDENTIALS_HELPER" own >"$WORK/delivered.json"
	OPENCODE_CONFIGURE_PLAN="$WORK/plan.json" \
		OPENCODE_CONFIGURE_HOME="$HOME" \
		OPENCODE_CONFIGURE_CREDENTIALS_PATH="$CREDENTIALS_PATH" \
		OPENCODE_CONFIGURE_DELIVERED="$WORK/delivered.json" \
		OPENCODE_CONFIGURE_SETTINGS_PATHS="$SETTINGS_PATHS" \
		OPENCODE_CONFIGURE_MODEL_PREFERENCE_PATH="$MODEL_PREFERENCE_PATH" \
		OPENCODE_CONFIGURE_SETTINGS_PATH="$SETTINGS_PATH" \
		OPENCODE_CONFIGURE_WEB_SEARCH="$WEB_SEARCH" \
		OPENCODE_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const path = require('path');
		const plan = JSON.parse(fs.readFileSync(process.env.OPENCODE_CONFIGURE_PLAN, 'utf8'));
		const secrets = plan.map((item) => {
			const secret = { envName: item.envName, name: `opencode: ${item.provider}`, type: item.type };
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
				path: process.env.OPENCODE_CONFIGURE_CREDENTIALS_PATH,
				template: true,
				content: JSON.stringify(JSON.parse(fs.readFileSync(process.env.OPENCODE_CONFIGURE_DELIVERED, 'utf8')), null, 2) + '\n',
			});
		}
		// Settings are returned literally, as opencode wrote them: they are the
		// user's, and a {env:...} reference in one is opencode's to expand.
		for (const rel of process.env.OPENCODE_CONFIGURE_SETTINGS_PATHS.split(' ').filter(Boolean)) {
			try {
				files.push({ path: rel, content: fs.readFileSync(path.join(process.env.OPENCODE_CONFIGURE_HOME, rel), 'utf8') });
			} catch (err) {
				// Not written in this session or any before it; nothing to keep.
			}
		}
		// The model picked with /models lives in opencode's state. It seeds a
		// sandbox's first launch and is the sandbox's own afterwards, so a later
		// pick in one sandbox is not reset at its next start.
		try {
			const rel = process.env.OPENCODE_CONFIGURE_MODEL_PREFERENCE_PATH;
			files.push({ path: rel, createOnly: true, content: fs.readFileSync(path.join(process.env.OPENCODE_CONFIGURE_HOME, rel), 'utf8') });
		} catch (err) {
			// No model was ever picked.
		}
		// Discobox's settings: the answer just given, and everything else as it
		// arrived — a setting someone added by editing the file is theirs.
		const settingsPath = process.env.OPENCODE_CONFIGURE_SETTINGS_PATH;
		let settings = {};
		try {
			settings = JSON.parse(fs.readFileSync(path.join(process.env.OPENCODE_CONFIGURE_HOME, settingsPath), 'utf8')) || {};
		} catch (err) {
			settings = {};
		}
		settings.webSearch = process.env.OPENCODE_CONFIGURE_WEB_SEARCH === 'true';
		delete settings.judgeModel;
		files.push({ path: settingsPath, content: JSON.stringify(settings, null, 2) + '\n' });
		fs.writeFileSync(process.env.OPENCODE_CONFIGURE_OUTPUT, JSON.stringify({ files, secrets }));
	NODE_EOF
}

SEEDED_LABELS=""
seed_previous_credentials

while :; do
	printf '%s\n' "${C_BOLD}${C_WARN}=============================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up opencode — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}=============================================================${C_RESET}"
	echo
	echo "Discobox is about to start opencode so you can set it up. This is a"
	echo "throwaway setup sandbox: it exists only to capture your providers and"
	echo "settings, and it is deleted the moment you leave."
	printf '%s\n' "${C_BOLD}Do not start real work in here — none of it is kept.${C_RESET}"
	echo
	if [ -n "$SEEDED_LABELS" ]; then
		printf '%s\n' "${C_BOLD}Already connected: $SEEDED_LABELS.${C_RESET}"
		echo "Change whatever you like — settings, the model, or the providers."
		echo
	fi
	printf '%s\n' "${C_BOLD}You must do both of these:${C_RESET}"
	echo
	printf '%s\n' "  1. ${C_BOLD}${C_CMD}/connect${C_RESET}  Connect each provider this harness should use —"
	echo "              as many as you like. This sandbox has no browser: a"
	echo "              sign-in prints a link to open on your own machine."
	echo "              ChatGPT and DigitalOcean sign-ins are forwarded back in"
	echo "              here; if Discobox warned their port is taken, pick the"
	echo "              headless method, or use an API key. A browser sign-in"
	echo "              on any other port cannot reach back in here: use a key."
	if [ -n "$SEEDED_LABELS" ]; then
		echo "              (Optional now: providers you already connected are kept.)"
	fi
	printf '%s\n' "  2. ${C_BOLD}${C_CMD}/exit${C_RESET}     Leave opencode when you're done. Setup only finishes"
	echo "              once you exit — staying in blocks it."
	echo
	echo "Worth doing while you're in there:"
	echo
	printf '%s\n' "  ${C_CMD}/models${C_RESET}     Pick the model this harness starts with"
	printf '%s\n' "  ${C_CMD}/themes${C_RESET}     Appearance"
	echo
	confirm_launch

	# opencode needs a real TTY for its interactive UI; run it under script.
	set +e
	# --standalone, as the launcher runs it: a background service would
	# outlive this script and keep the store open behind it.
	script -q -e -c 'opencode --standalone' /dev/null
	opencode_status=$?
	set -e
	# opencode can leave its full-screen UI's contents behind when it exits,
	# and everything printed next would be drawn over them.
	# Only after a clean exit: when opencode fails, what it printed is the
	# explanation, and clearing would erase it.
	if [ "$opencode_status" -eq 0 ] && [ -t 1 ]; then
		printf '\033[?1049l\033[H\033[2J'
	fi
	echo

	count=$(classify_credentials)

	if [ "$count" -eq 0 ]; then
		if [ "$opencode_status" -ne 0 ]; then
			# Not "the user connected nothing": opencode refused to run at all,
			# and saying so points at the message it printed just above.
			printf '%s\n' "${C_ERR}${C_BOLD}opencode exited with status $opencode_status without connecting a provider.${C_RESET}" >&2
		else
			printf '%s\n' "${C_ERR}${C_BOLD}No provider is connected.${C_RESET}" >&2
			echo "opencode still runs without one — with its free models, a local server, or a"
			echo "provider connected inside a sandbox — but this harness will hold no credentials."
		fi
		if confirm_retry "saves your settings with no provider"; then
			continue
		fi
		break
	fi

	echo "Connected: $(plan_labels). Checking the default model…"
	if verify_default_model; then
		echo "The default model answered."
		break
	fi
	echo
	printf '%s\n' "${C_ERR}${C_BOLD}opencode's default model did not answer.${C_RESET}" >&2
	echo "Your providers are still connected. Start opencode again to pick another"
	echo "model with /models, or reconnect the provider with /connect."
	if confirm_retry "saves everything as it is"; then
		continue
	fi
	break
done

print_notices
ask_web_search
write_output

echo
echo "opencode configuration complete."
