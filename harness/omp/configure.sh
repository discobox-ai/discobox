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
# The default model is checked with a one-line `omp -p`; a failure is reported
# rather than refused (see verify_default_model).
#
# No credential is exported as an environment variable. The script returns the
# export document as a credential document with each credential's sentinel
# in place of its secret, which the image's launcher imports into omp at every
# launch.
#
# It also returns omp's settings as the user left them (config.yml: the default
# model, theme and the rest).
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each one's value is
# available as $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real
# credential on the way out — so the previous document is imported with
# sentinels and the session opens already signed in. A credential still
# holding its sentinel afterwards was not replaced, and is reported back as
# usePrevious rather than as a value.
set -eu
HARNESS_CAPTURE_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
export HARNESS_CAPTURE_DIR

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json

# Home-relative, like every harness file.
CREDENTIALS_PATH=".config/discobox/omp-credentials.json"
OMP_SETTINGS_PATH=".omp/agent/config.yml"
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
# previous run captured with each PREV_ sentinel in place of its reference
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
		let reference = {};
		try {
			reference = JSON.parse(kept && typeof kept.content === 'string' ? kept.content : '{}') || {};
		} catch (err) {
			reference = {};
		}
		const action = /"\$secret"\s*:\s*"([A-Za-z_][A-Za-z0-9_]*)"/g;
		const credentials = [];
		for (const entry of Array.isArray(reference.credentials) ? reference.credentials : []) {
			const text = JSON.stringify(entry);
			const names = [...text.matchAll(action)].map((m) => m[1]);
			if (names.length === 0 || names.some((name) => !process.env['PREV_' + name])) continue;
			credentials.push(JSON.parse(text, (key, value) => value && typeof value === 'object' && Object.keys(value).length === 1 && typeof value.$secret === 'string' ? process.env['PREV_' + value.$secret] : value));
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
		const path = require('path');
		const dir = process.env.HARNESS_CAPTURE_DIR;
		const { normalize } = require(path.join(dir, 'omp-normalize.cjs'));
		const definition = require(path.join(dir, 'omp-image.json')).harness.saveBack;
		const read = (p, fallback) => { try { return JSON.parse(fs.readFileSync(p, 'utf8')); } catch { return fallback; } };
		const sentinels = Object.fromEntries(Object.entries(process.env).filter(([k]) => k.startsWith('PREV_')).map(([k,v]) => [k.slice(5),v]));
		const result = normalize(read(process.env.OMP_CONFIGURE_EXPORT, {}), read(process.env.OMP_CONFIGURE_PREVIOUS, {}), sentinels, definition);
		fs.writeFileSync(process.env.OMP_CONFIGURE_PLAN, JSON.stringify(result.plan));
		process.stdout.write(String(result.plan.length));
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
# document that delivers them, and omp's settings as the user left them.
# Secret shapes:
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
			// a reference, and each sandbox renders its own sentinels into it.
			files.push({
				path: process.env.OMP_CONFIGURE_CREDENTIALS_PATH,

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
	else
		echo
		printf '%s\n' "${C_ERR}${C_BOLD}omp's default model did not answer.${C_RESET}" >&2
		echo "Your sign-ins are kept. Start omp again to pick another model with"
		echo "/model, or sign in again with /login."
	fi
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
