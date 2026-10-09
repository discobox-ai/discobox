#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# copilot Definition). Signs Copilot in, opens it so the user can set it up the
# way they normally would, then reads back what Copilot itself stored to capture
# the result. Writes it to /run/discobox/configure/harness-configure.json for
# discobox to apply to the HarnessConfig.
#
# Copilot authenticates with a GitHub token, and there are two ways to get one
# (ADR 26-10-02-840 §2), offered in this order:
#   - A fine-grained personal access token with only the "Copilot Requests"
#     permission, read by Copilot's own `copilot login --with-token`. It can
#     reach Copilot and nothing else on GitHub.
#   - /login inside Copilot. Its token is a GitHub OAuth App token with `repo`
#     scope among others, and the secret it becomes cannot be held to one host
#     — Copilot sends it to github.com and to githubcopilot.com — so every
#     discobox using this harness could act on every repository the account
#     can. The user is told so and has to say yes.
# Either way Copilot stores the token, and with no keychain in a sandbox it
# stores it in ~/.copilot/config.json, as authTokens["<host>:<login>"] beside
# lastLoggedInUser. That is where this script reads it from.
#
# It is returned as one `token` secret, COPILOT_GITHUB_TOKEN — the variable
# Copilot documents for headless use, which outranks any stored login. Neither
# sign-in yields anything to refresh, so it is never `oauth`. Whichever the
# user picked, it is verified with a tool-free `copilot -p` before being
# accepted.
#
# It also returns ~/.copilot/settings.json, minus storeTokenPlaintext (see
# allow_plaintext_storage), so anything the user changes in the session —
# default model, theme, status line — becomes the harness's default going
# forward. ~/.copilot/config.json is never returned: Copilot calls it "managed
# automatically", and besides the token it holds this sandbox's own state.
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

# COPILOT_HOME is Copilot's own override; default to the same ~/.copilot the
# harness files are delivered to.
COPILOT_HOME_DIR="${COPILOT_HOME:-$HOME/.copilot}"
SETTINGS_FILE="$COPILOT_HOME_DIR/settings.json"
STATE_FILE="$COPILOT_HOME_DIR/config.json"
SETTINGS_PATH=".copilot/settings.json"

TOKEN_ENV=COPILOT_GITHUB_TOKEN

# The only host the harness can deliver a token for: an Enterprise Cloud
# (data residency) login also needs COPILOT_GH_HOST in every sandbox, which a
# configure command cannot set (ADR 26-10-02-840 §3).
GITHUB_HOST="https://github.com"

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

# token_label prints the human name recorded alongside the secret, from the
# token's own prefix — what the token is, not how it was entered: a gho_ OAuth
# token pasted at the PAT prompt (gh's, say) carries the same repository access
# a /login does.
token_label() {
	case "$1" in
	github_pat_*) echo "GitHub fine-grained personal access token" ;;
	gho_*) echo "GitHub OAuth token (repository access)" ;;
	ghu_*) echo "GitHub App user token" ;;
	*) echo "GitHub token" ;;
	esac
}

# previous_label prints the name the previous run recorded for its credential,
# which says which of the two sign-ins it was; the sentinel's own prefix would
# say the same, but the recorded name is what the user saw last time.
previous_label() {
	COPILOT_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		COPILOT_CONFIGURE_ENV_NAME="$TOKEN_ENV" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.COPILOT_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const secret = (previous.secrets || []).find((s) => s && s.envName === process.env.COPILOT_CONFIGURE_ENV_NAME);
		process.stdout.write((secret && secret.name) || 'GitHub token');
	NODE_EOF
}

# previous_sentinel prints the PREV_ sentinel of the credential a previous
# configure run stored, or nothing. Only a secret the seed lists and whose PREV_
# variable is actually set counts: a seeded secret with no value behind it
# cannot be reused.
previous_sentinel() {
	[ -f "$PREVIOUS_CONFIG" ] || return 0
	COPILOT_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		COPILOT_CONFIGURE_ENV_NAME="$TOKEN_ENV" node <<-'NODE_EOF'
		const fs = require('fs');
		let previous = {};
		try {
			previous = JSON.parse(fs.readFileSync(process.env.COPILOT_CONFIGURE_PREVIOUS, 'utf8')) || {};
		} catch (err) {
			previous = {};
		}
		const envName = process.env.COPILOT_CONFIGURE_ENV_NAME;
		const known = (previous.secrets || []).some((s) => s && s.envName === envName);
		const sentinel = process.env['PREV_' + envName];
		if (known && sentinel) {
			process.stdout.write(sentinel);
		}
	NODE_EOF
}

# allow_plaintext_storage sets storeTokenPlaintext in Copilot's settings. A
# sandbox has no keychain, and without the setting Copilot stops a sign-in at a
# consent question — or, reading a token from a pipe, does not store it at all —
# and this script would find nothing to capture. write_output strips it again:
# a running sandbox never stores a token, since it gets one in the environment.
#
# Copilot writes these files as JSON with `//` comment lines heading them, so
# comment lines are dropped before parsing; the settings themselves are JSON.
allow_plaintext_storage() {
	COPILOT_CONFIGURE_SETTINGS_FILE="$SETTINGS_FILE" node <<-'NODE_EOF'
		const fs = require('fs');
		const nodePath = require('path');
		const file = process.env.COPILOT_CONFIGURE_SETTINGS_FILE;
		let settings = {};
		try {
			const body = fs.readFileSync(file, 'utf8').split('\n').filter((l) => !/^\s*\/\//.test(l)).join('\n');
			settings = JSON.parse(body) || {};
		} catch (err) {
			settings = {};
		}
		settings.storeTokenPlaintext = true;
		fs.mkdirSync(nodePath.dirname(file), { recursive: true });
		fs.writeFileSync(file, JSON.stringify(settings, null, 2) + '\n');
	NODE_EOF
}

# clear_captured_credential removes Copilot's state file, which is where a
# stored token lives, so a retry's detection cannot mistake a token from an
# earlier attempt in this same sandbox for a fresh one. Nothing else in it is
# worth keeping here: trust comes from COPILOT_ALLOW_ALL in the image's env,
# and the rest is state Copilot regenerates.
clear_captured_credential() {
	rm -f "$STATE_FILE"
}

# stored_token echoes the token Copilot stored for the account it last signed
# in, and fails if there is none. A login to any host but github.com fails with
# status 4, after saying why.
stored_token() {
	[ -f "$STATE_FILE" ] || return 1
	COPILOT_CONFIGURE_STATE_FILE="$STATE_FILE" \
		COPILOT_CONFIGURE_HOST="$GITHUB_HOST" node <<-'NODE_EOF'
		const fs = require('fs');
		let state = {};
		try {
			const body = fs.readFileSync(process.env.COPILOT_CONFIGURE_STATE_FILE, 'utf8').split('\n').filter((l) => !/^\s*\/\//.test(l)).join('\n');
			state = JSON.parse(body) || {};
		} catch (err) {
			process.exit(3);
		}
		const user = state.lastLoggedInUser || {};
		if (typeof user.host !== 'string' || typeof user.login !== 'string') {
			process.exit(3);
		}
		if (user.host.replace(/\/+$/, '') !== process.env.COPILOT_CONFIGURE_HOST) {
			process.stderr.write(`Copilot signed in to ${user.host}, and this harness can only run Copilot against ${process.env.COPILOT_CONFIGURE_HOST}.\n`);
			process.exit(4);
		}
		// Copilot keeps the token under the account's key, and under the same key
		// with the provider appended; either is the one token.
		const tokens = state.authTokens || {};
		const key = `${user.host}:${user.login}`;
		const entry = tokens[key] || tokens[`${key}:github`] || {};
		if (typeof entry.token !== 'string' || !entry.token) {
			process.exit(3);
		}
		process.stdout.write(entry.token);
	NODE_EOF
}

# confirm_retry asks whether to start over after an attempt produced no usable
# credential. Every retry goes through here so the loop can only turn when a
# person asks it to: an attempt that fails without reaching the user -- Copilot
# refusing to start, say -- fails again the moment it is retried, and looping
# on that is a busy loop, not a retry. End of input means nobody is there to
# answer, which fails the configure flow rather than spinning.
confirm_retry() {
	printf 'Try again? [Y/n] '
	if ! read -r retry_choice; then
		echo >&2
		echo "No input; aborting without configuring Copilot." >&2
		exit 1
	fi
	echo
	case "${retry_choice:-y}" in
	[nN]*)
		echo "Aborting without configuring Copilot." >&2
		exit 1
		;;
	esac
}

# choose_sign_in sets METHOD to keep, token or login. End of input is not a
# choice: nobody is there, and starting an interactive TUI at nobody wedges the
# configure flow rather than failing it.
choose_sign_in() {
	METHOD=""
	while [ -z "$METHOD" ]; do
		if [ -n "$SEEDED_SENTINEL" ]; then
			printf '%s' "${C_BOLD}Press Enter to keep it and start Copilot, or choose 1 or 2 to replace it:${C_RESET} "
		else
			printf '%s' "${C_BOLD}Choose 1 or 2 [1]:${C_RESET} "
		fi
		if ! read -r method_choice; then
			echo >&2
			echo "No input; aborting without configuring Copilot." >&2
			exit 1
		fi
		echo
		case "$method_choice" in
		"")
			if [ -n "$SEEDED_SENTINEL" ]; then METHOD=keep; else METHOD=token; fi
			;;
		1) METHOD=token ;;
		2) confirm_broad_token "A /login sign-in" "Sign in with /login anyway?" && METHOD=login ;;
		*) echo "Choose 1 or 2." >&2 ;;
		esac
	done
}

# confirm_broad_token is the warning a token that is not a fine-grained PAT has
# to clear, whether it comes from /login or is pasted at the PAT prompt: it can
# reach every repository the account can, and nothing holds the harness's
# secret to Copilot alone. Only an explicit yes goes on. $1 is what the token
# is, $2 the question.
confirm_broad_token() {
	printf '%s\n' "${C_ERR}${C_BOLD}$1 gives every discobox that uses this harness your GitHub"
	printf '%s\n' "account's reach: its token can read and push to every repository you can,"
	printf '%s\n' "and the agent can use it for that, not only for Copilot.${C_RESET}"
	echo "A fine-grained token with only \"Copilot Requests\" (option 1) cannot."
	printf '%s [y/N] ' "$2"
	if ! read -r login_choice; then
		echo >&2
		echo "No input; aborting without configuring Copilot." >&2
		exit 1
	fi
	echo
	case "$login_choice" in
	[yY]*) return 0 ;;
	esac
	return 1
}

# sign_in_with_token hands the token to Copilot's own `copilot login
# --with-token`, which reads it without echoing it from the terminal, checks
# it with GitHub, and stores it where stored_token reads it.
sign_in_with_token() {
	echo "Create one at https://github.com/settings/personal-access-tokens/new"
	echo "with the account permission \"Copilot Requests\" and nothing else, then paste it."
	echo
	env -u COPILOT_GITHUB_TOKEN -u GH_TOKEN -u GITHUB_TOKEN copilot login --with-token
}

# run_copilot opens Copilot for the user. With the previous credential kept, its
# sentinel is Copilot's token for the session, so it opens signed in; otherwise
# no token variable is passed, so Copilot uses what it stored and nothing in
# the environment can shadow a /login.
run_copilot() {
	set +e
	if [ "$METHOD" = keep ]; then
		env -u GH_TOKEN -u GITHUB_TOKEN COPILOT_GITHUB_TOKEN="$SEEDED_SENTINEL" \
			script -q -e -c 'copilot --allow-all' /dev/null
	else
		env -u COPILOT_GITHUB_TOKEN -u GH_TOKEN -u GITHUB_TOKEN \
			script -q -e -c 'copilot --allow-all' /dev/null
	fi
	copilot_status=$?
	set -e
	echo
}

# verify_credential runs a trivial, tool-free prompt with only the chosen token
# in the environment, which is exactly what a real sandbox will run with. It
# runs in a COPILOT_HOME of its own, so the stored login cannot stand in for the
# token being checked.
verify_credential() {
	verify_home=$(mktemp -d)
	verify_log=$(mktemp)
	set +e
	verify_reply=$(cd "$verify_home" && env -u GH_TOKEN -u GITHUB_TOKEN -u COPILOT_ALLOW_ALL \
		COPILOT_GITHUB_TOKEN="$1" COPILOT_HOME="$verify_home" \
		timeout 180 copilot --silent --no-auto-update --no-custom-instructions --disable-builtin-mcps \
		--available-tools=discobox-no-tools --prompt='Reply with exactly: discobox-ok' 2>"$verify_log")
	verify_status=$?
	set -e
	if [ "$verify_status" -ne 0 ] || [ -z "$verify_reply" ]; then
		echo
		echo "The credential did not work:" >&2
		tail -n 5 "$verify_log" >&2
		rm -rf "$verify_home" "$verify_log"
		return 1
	fi
	rm -rf "$verify_home" "$verify_log"
	echo "Copilot replied: $verify_reply"
	return 0
}

# write_output records the result: the token as a secret, plus the settings the
# session left. Secret shapes:
#   - keep previous:  usePrevious marker, no value (KEEP_PREVIOUS set).
#   - new sign-in:    a plain token { token }.
write_output() {
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	COPILOT_CONFIGURE_ENV_NAME="$TOKEN_ENV" \
		COPILOT_CONFIGURE_NAME="$LABEL" \
		COPILOT_CONFIGURE_TOKEN="${OUTPUT_TOKEN:-}" \
		COPILOT_CONFIGURE_KEEP_PREVIOUS="${KEEP_PREVIOUS:-}" \
		COPILOT_CONFIGURE_SETTINGS_FILE="$SETTINGS_FILE" \
		COPILOT_CONFIGURE_SETTINGS_PATH="$SETTINGS_PATH" \
		COPILOT_CONFIGURE_OUTPUT="$OUTPUT" node <<-'NODE_EOF'
		const fs = require('fs');
		const secret = {
			envName: process.env.COPILOT_CONFIGURE_ENV_NAME,
			name: process.env.COPILOT_CONFIGURE_NAME,
			type: 'token',
		};
		if (process.env.COPILOT_CONFIGURE_KEEP_PREVIOUS) {
			secret.usePrevious = true;
		} else {
			secret.value = { token: process.env.COPILOT_CONFIGURE_TOKEN };
		}

		// The settings as the user left them, minus the one this script set. A
		// sandbox gets its token in the environment and stores none, and a
		// harness default to store tokens in plain text is not one to hand on.
		const files = [];
		let settings = null;
		try {
			const body = fs.readFileSync(process.env.COPILOT_CONFIGURE_SETTINGS_FILE, 'utf8').split('\n').filter((l) => !/^\s*\/\//.test(l)).join('\n');
			settings = JSON.parse(body);
		} catch (err) {
			settings = null;
		}
		if (settings && typeof settings === 'object') {
			delete settings.storeTokenPlaintext;
			if (Object.keys(settings).length > 0) {
				files.push({
					path: process.env.COPILOT_CONFIGURE_SETTINGS_PATH,
					content: JSON.stringify(settings, null, 2) + '\n',
				});
			}
		}
		fs.writeFileSync(process.env.COPILOT_CONFIGURE_OUTPUT, JSON.stringify({ files, secrets: [secret] }));
	NODE_EOF
}

SEEDED_SENTINEL=$(previous_sentinel)
PREVIOUS_LABEL=""
if [ -n "$SEEDED_SENTINEL" ]; then
	PREVIOUS_LABEL=$(previous_label)
fi

allow_plaintext_storage

TOKEN=""
LABEL=""
KEEP_PREVIOUS=""
METHOD=""

while [ -z "$TOKEN" ]; do
	# A token that is not a fine-grained PAT the user already said yes to this
	# round, so the capture below does not ask a second time.
	BROAD_ACCEPTED=""
	# A configured harness delivers no config.json, but an attempt earlier in
	# this sandbox may have left one; every round starts from nothing stored.
	clear_captured_credential

	printf '%s\n' "${C_BOLD}${C_WARN}=================================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up GitHub Copilot — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}=================================================================${C_RESET}"
	echo
	echo "Discobox is about to start Copilot so you can set it up. This is a"
	echo "throwaway setup sandbox: it exists only to capture your sign-in and"
	echo "settings, and it is deleted the moment you leave."
	printf '%s\n' "${C_BOLD}Do not start real work in here — none of it is kept.${C_RESET}"
	echo
	if [ -n "$SEEDED_SENTINEL" ]; then
		printf '%s\n' "${C_BOLD}You are already signed in ($PREVIOUS_LABEL).${C_RESET}"
		echo
	fi
	printf '%s\n' "${C_BOLD}How should Copilot sign in?${C_RESET}"
	echo
	printf '%s\n' "  1. ${C_BOLD}A fine-grained personal access token${C_RESET} (recommended). With only the"
	echo "     \"Copilot Requests\" permission it reaches Copilot and nothing else."
	printf '%s\n' "  2. ${C_BOLD}${C_CMD}/login${C_RESET} inside Copilot, in your browser. Its token can also act on"
	echo "     every repository your account can, from inside every discobox."
	echo
	choose_sign_in

	if [ "$METHOD" = token ]; then
		if ! sign_in_with_token; then
			printf '%s\n' "${C_ERR}${C_BOLD}Copilot did not accept that token.${C_RESET}" >&2
			confirm_retry
			continue
		fi
		echo
		# The prompt asks for a fine-grained PAT, but Copilot takes any token
		# it supports — gh's own OAuth token among them — and those carry the
		# same repository access a /login does.
		pasted=$(stored_token 2>/dev/null) || pasted=""
		case "$pasted" in
		github_pat_* | "") ;;
		*)
			if ! confirm_broad_token "That is not a fine-grained token: it" "Use it anyway?"; then
				clear_captured_credential
				confirm_retry
				continue
			fi
			BROAD_ACCEPTED="$pasted"
			;;
		esac
	fi

	echo "Then, in Copilot:"
	echo
	if [ "$METHOD" = login ]; then
		printf '%s\n' "  1. ${C_BOLD}${C_CMD}/login${C_RESET}   Sign in. Nothing can be saved without it. Open the link it"
		echo "              prints in your own browser: the port it calls back to is"
		echo "              forwarded from your machine, within about 20 seconds. If"
		echo "              the browser cannot connect after you authorize, reload"
		echo "              that page. \"Sign in with a device code\" works too."
		printf '%s\n' "  2. ${C_BOLD}${C_CMD}/exit${C_RESET}    Leave Copilot when you're done. Setup only finishes"
		echo "              once you exit — staying in blocks it."
	else
		printf '%s\n' "  ${C_BOLD}${C_CMD}/exit${C_RESET}    Leave Copilot when you're done. Setup only finishes once"
		echo "           you exit — staying in blocks it."
	fi
	echo
	echo "Worth doing while you're in there:"
	echo
	# /config model sets the default every later session starts on; a bare
	# /model picks one for this session alone, which leaves with this sandbox.
	# "model" is part of the command, not a placeholder.
	printf '%s\n' "  ${C_CMD}/config model${C_RESET}  Pick the default model this harness runs with"
	echo "                 (type it as shown; /model alone changes this session only)"
	printf '%s\n' "  ${C_CMD}/settings${C_RESET}      Theme, status line, and the rest"
	echo
	printf '%s' "${C_BOLD}Press Enter to start Copilot.${C_RESET} "
	if ! read -r _launch_ack; then
		echo >&2
		echo "No input; aborting without configuring Copilot." >&2
		exit 1
	fi
	echo

	# Copilot needs a real TTY for its interactive UI; run it under script.
	run_copilot

	stored_status=0
	stored=$(stored_token) || stored_status=$?
	if [ "$stored_status" -eq 4 ]; then
		confirm_retry
		continue
	fi
	if [ -n "$stored" ] && [ "$stored" != "$SEEDED_SENTINEL" ]; then
		# Whatever was chosen before Copilot started, the session itself can
		# run /login — from the PAT path or a kept sign-in — and store a token
		# with the account's repository access. What is stored is what is
		# saved, so it clears the warning here unless it already did: the
		# /login path, or the very token pasted and accepted above.
		case "$stored" in
		github_pat_*) ;;
		*)
			if [ "$METHOD" != login ] && [ "$stored" != "$BROAD_ACCEPTED" ] &&
				! confirm_broad_token "Copilot signed in with a token that is not a fine-grained PAT: it" "Keep it anyway?"; then
				clear_captured_credential
				confirm_retry
				continue
			fi
			;;
		esac
		TOKEN="$stored"
		LABEL=$(token_label "$stored")
		KEEP_PREVIOUS=""
	elif [ "$METHOD" = keep ]; then
		TOKEN="$SEEDED_SENTINEL"
		LABEL="$PREVIOUS_LABEL"
		KEEP_PREVIOUS=yes
	elif [ "$copilot_status" -ne 0 ]; then
		# Not "the user skipped the sign-in": Copilot refused to run at all, and
		# saying so points at the message it printed just above rather than
		# asking again as if a step had been missed.
		printf '%s\n' "${C_ERR}${C_BOLD}Copilot exited with status $copilot_status without signing in.${C_RESET}" >&2
		confirm_retry
		continue
	else
		printf '%s\n' "${C_ERR}${C_BOLD}You left Copilot without signing in, so there is nothing to" >&2
		printf '%s\n' "save. Setup needs you to sign in before you exit.${C_RESET}" >&2
		confirm_retry
		continue
	fi

	if [ -n "$KEEP_PREVIOUS" ]; then
		echo "The credential is unchanged; checking it still works…"
	else
		echo "Verifying the credential…"
	fi
	if ! verify_credential "$TOKEN"; then
		echo
		if [ -n "$KEEP_PREVIOUS" ]; then
			# The existing credential is the thing that failed -- revoked, most
			# likely. Stop offering it: another round would keep it again,
			# offering a retry that cannot succeed until the user signs in afresh.
			printf '%s\n' "${C_ERR}${C_BOLD}The existing credential no longer works. Sign in again to replace it.${C_RESET}" >&2
			SEEDED_SENTINEL=""
		fi
		TOKEN=""
		LABEL=""
		KEEP_PREVIOUS=""
		confirm_retry
	fi
done

if [ -n "$KEEP_PREVIOUS" ]; then
	OUTPUT_TOKEN="" write_output
else
	OUTPUT_TOKEN="$TOKEN" write_output
fi

echo
echo "GitHub Copilot configuration complete ($LABEL)."
