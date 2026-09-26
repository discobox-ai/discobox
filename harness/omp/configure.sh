#!/bin/sh
# Runs as the configure sandbox's primary terminal (see harness.Configure on the
# omp Definition). Launches omp interactively, lets the user sign in to
# whichever providers they want and configure it however they normally would,
# then reads what omp itself stored to capture the result. Writes it to
# /run/discobox/configure/harness-configure.json for discobox to apply to the
# HarnessConfig.
#
# omp keeps every credential `/login` collects in its SQLite store,
# ~/.omp/agent/agent.db: an OAuth sign-in as its token set, an API key as the
# key. Neither is a file a harness can deliver, so each kind takes the route
# omp itself supports:
#   - An OAuth sign-in is returned in .omp/agent/discobox-auth.json, a
#     templated file in the shape of `omp auth-broker import`, with the
#     sentinel where the access token goes; the launcher imports it (see
#     import.sh). One the control plane knows how to renew (see oauthRefresh
#     below) is an `oauth` secret with its refresh material; any other is a
#     `token` secret holding the access token, and the script says when it
#     expires, because nothing will renew it.
#   - An API key is a `token` secret exported as OMP_<PROVIDER>_CREDENTIAL,
#     and .omp/agent/models.yml names that variable as the provider's apiKey,
#     which omp resolves from the environment. The file is the user's own
#     custom-provider file with those entries added.
# The default model is checked with a one-line `omp --print`, and a failure
# is reported rather than refused (see verify_default_model).
#
# No credential is exported under a name omp reads on its own: the sentinel
# reaches omp through the import or through models.yml, and never as an
# ANTHROPIC_API_KEY-style variable a stray `omp` could pick up unconfigured.
#
# It also returns omp's settings as the user left them — config.yml (model
# roles, theme, approvals) — so they become the harness's defaults.
#
# Reconfigure: /run/discobox/configure/harness-previous-config.json lists the
# secrets a previous run stored, without their values. Each one's value is
# available as $PREV_<ENV_NAME> — a sentinel the proxy swaps for the real
# credential on the way out — so the previous sign-ins are imported back with
# sentinels, the previous API keys re-pointed at their PREV_ variables, and the
# session opens already signed in. A credential still holding its sentinel
# afterwards was not replaced, and is reported back as usePrevious rather than
# as a value.
set -eu

PREVIOUS_CONFIG=/run/discobox/configure/harness-previous-config.json
OUTPUT=/run/discobox/configure/harness-configure.json
IMPORT=/usr/local/libexec/discobox/omp-import-credentials

# Home-relative, like every harness file. omp's agent directory is ~/.omp/agent
# unless PI_CODING_AGENT_DIR moves it, which nothing in this image does.
AGENT_DIR=".omp/agent"
DB_FILE="$HOME/$AGENT_DIR/agent.db"
AUTH_PATH="$AGENT_DIR/discobox-auth.json"
AUTH_FILE="$HOME/$AUTH_PATH"
MODELS_PATH="$AGENT_DIR/models.yml"
MODELS_FILE="$HOME/$MODELS_PATH"
SETTINGS_PATHS="$AGENT_DIR/config.yml"
# Discobox's own settings for this harness: the judge model. The image declares
# a baseline, and a configured harness delivers its own copy into this
# sandbox, which is what a reconfigure starts from.
SETTINGS_PATH=".config/discobox/omp-harness.json"

# The expiry a delivered OAuth entry carries when the control plane renews its
# token: far enough out that omp never decides the credential is stale and
# tries a refresh itself. That is not a lie about the real token — what the
# entry carries is a sentinel, which genuinely does not go stale, and the
# refresh token it would need never leaves the control plane. The importer
# takes it as a date.
FAR_FUTURE_EXPIRY=4102444800000
FAR_FUTURE_DATE="2100-01-01T00:00:00.000Z"
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

# The node helpers below share these, and models.yml is YAML that omp reads
# and JSON that this script writes: JSON is YAML, so a file this script wrote
# parses as JSON, and one a person wrote as YAML is parsed by bun, which the
# image ships omp on. A file neither can read is left as it is (see
# write_output).
node_env() {
	OMP_CONFIGURE_PREVIOUS="$PREVIOUS_CONFIG" \
		OMP_CONFIGURE_AUTH_PATH="$AUTH_PATH" \
		OMP_CONFIGURE_AUTH_FILE="$AUTH_FILE" \
		OMP_CONFIGURE_MODELS_PATH="$MODELS_PATH" \
		OMP_CONFIGURE_MODELS_FILE="$MODELS_FILE" \
		OMP_CONFIGURE_DB_FILE="$DB_FILE" \
		OMP_CONFIGURE_WORK="$WORK" \
		OMP_CONFIGURE_HOME="$HOME" \
		OMP_CONFIGURE_FAR_FUTURE_EXPIRY="$FAR_FUTURE_EXPIRY" \
		OMP_CONFIGURE_FAR_FUTURE_DATE="$FAR_FUTURE_DATE" \
		OMP_CONFIGURE_REFRESH_PLACEHOLDER="$REFRESH_PLACEHOLDER" \
		OMP_CONFIGURE_SETTINGS_PATHS="$SETTINGS_PATHS" \
		OMP_CONFIGURE_SETTINGS_PATH="$SETTINGS_PATH" \
		OMP_CONFIGURE_OUTPUT="$OUTPUT" \
		node "$@"
}

# The JavaScript the helpers share: reading the previous configuration, and
# models.yml as JSON or, through bun, as YAML.
COMMON_JS='
	const fs = require("fs");
	const path = require("path");
	const { spawnSync } = require("child_process");
	const readJSON = (file, fallback) => {
		try { return JSON.parse(fs.readFileSync(file, "utf8")) ?? fallback; } catch (err) { return fallback; }
	};
	const previous = readJSON(process.env.OMP_CONFIGURE_PREVIOUS, {});
	const previousFile = (rel) => (previous.files || []).find((f) => f && f.path === rel);
	// readModels returns models.yml as an object, or null when there is no
	// file, or undefined when there is one nobody here can read.
	const readModels = () => {
		let text;
		try { text = fs.readFileSync(process.env.OMP_CONFIGURE_MODELS_FILE, "utf8"); } catch (err) { return null; }
		if (text.trim() === "") return {};
		try { const parsed = JSON.parse(text); return parsed && typeof parsed === "object" ? parsed : undefined; } catch (err) {}
		const yaml = spawnSync("bun", ["-e", "process.stdout.write(JSON.stringify(Bun.YAML.parse(require(\"fs\").readFileSync(0, \"utf8\"))))"], { input: text, encoding: "utf8" });
		if (yaml.status !== 0) return undefined;
		try { const parsed = JSON.parse(yaml.stdout); return parsed && typeof parsed === "object" ? parsed : undefined; } catch (err) { return undefined; }
	};
	const writeModels = (models) => {
		fs.mkdirSync(path.dirname(process.env.OMP_CONFIGURE_MODELS_FILE), { recursive: true, mode: 0o700 });
		fs.writeFileSync(process.env.OMP_CONFIGURE_MODELS_FILE, JSON.stringify(models, null, 2) + "\n");
	};
	// Discobox names an API key variable OMP_<PROVIDER>_CREDENTIAL, and a
	// reconfigure re-points it at PREV_ of the same; nothing else in models.yml
	// is Discobox`s to touch.
	const ourVariable = /^(PREV_)?OMP_[A-Z0-9_]+_CREDENTIAL$/;
	const action = /\{\{\s*\.secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}/g;
'

# seed_previous_credentials writes what a previous run captured back where omp
# reads it, so the session below opens already signed in.
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
# An OAuth entry is rendered with its sentinel and imported into omp`s store
# (import.sh). An API key`s models.yml entry is re-pointed at its PREV_
# variable. A provider whose sentinel is not set — its secret was deleted, say
# — is left out, and opens signed out, which is the one case a sign-in is
# unavoidable; a models.yml entry naming a variable this sandbox does not have
# is removed with it, since omp would otherwise read the variable`s name as
# the key.
seed_previous_credentials() {
	node_env -e "$COMMON_JS"'
		const kept = previousFile(process.env.OMP_CONFIGURE_AUTH_PATH);
		const template = readJSONText(kept && typeof kept.content === "string" ? kept.content : "{}");
		function readJSONText(text) { try { return JSON.parse(text) || {}; } catch (err) { return {}; } }
		const seeded = {};
		const labels = [];
		for (const [provider, entry] of Object.entries(template)) {
			const text = JSON.stringify(entry);
			const names = [...text.matchAll(action)].map((m) => m[1]);
			if (names.length === 0 || names.some((name) => !process.env["PREV_" + name])) continue;
			seeded[provider] = JSON.parse(text.replace(action, (_, name) => process.env["PREV_" + name]));
			labels.push(provider);
		}
		const file = process.env.OMP_CONFIGURE_AUTH_FILE;
		if (Object.keys(seeded).length === 0) {
			fs.rmSync(file, { force: true });
		} else {
			fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
			fs.writeFileSync(file, JSON.stringify(seeded, null, 2) + "\n", { mode: 0o600 });
		}
		const models = readModels();
		if (models && models.providers && typeof models.providers === "object") {
			for (const [provider, spec] of Object.entries(models.providers)) {
				if (!spec || typeof spec !== "object" || typeof spec.apiKey !== "string" || !ourVariable.test(spec.apiKey)) continue;
				const name = spec.apiKey.replace(/^PREV_/, "");
				if (process.env["PREV_" + name]) {
					spec.apiKey = "PREV_" + name;
					labels.push(provider);
				} else {
					delete spec.apiKey;
					if (Object.keys(spec).length === 0) delete models.providers[provider];
				}
			}
			writeModels(models);
		}
		fs.writeFileSync(path.join(process.env.OMP_CONFIGURE_WORK, "seeded.txt"), labels.join(", "));
	'
	SEEDED_LABELS=$(cat "$WORK/seeded.txt")
	if [ -f "$AUTH_FILE" ]; then
		sh "$IMPORT" "$AUTH_FILE" || true
	fi
}

# stored_credentials prints omp`s active credentials as one JSON array of
# {provider, credential_type, data}: the rows `/login` wrote, and the ones
# seed_previous_credentials imported. A store with no credential table — no
# sign-in ever happened — is an empty array, as is no store at all.
stored_credentials() {
	if [ -f "$DB_FILE" ] && command -v sqlite3 >/dev/null 2>&1; then
		sqlite3 -json "$DB_FILE" \
			"select provider, credential_type, data from auth_credentials where disabled_cause is null order by provider, id" \
			2>/dev/null || echo '[]'
	else
		echo '[]'
	fi
}

# classify_credentials turns the store and models.yml into $WORK/plan.json:
# one entry per provider, with the secret to store (or the note that the
# previous one is kept) and how it is delivered. Prints how many there are.
#
# A credential whose secret material is still the PREV_ sentinel seeded for its
# name was not replaced, and is kept with the entry the last run returned.
# Anything else is a new credential and is captured. A provider with several
# accounts stored is captured as its first: one sentinel per provider is what
# the file can deliver.
classify_credentials() {
	stored_credentials >"$WORK/stored.json"
	node_env -e "$COMMON_JS"'
		const stored = readJSON(path.join(process.env.OMP_CONFIGURE_WORK, "stored.json"), []);
		const farFuture = Number(process.env.OMP_CONFIGURE_FAR_FUTURE_EXPIRY);
		const farFutureDate = process.env.OMP_CONFIGURE_FAR_FUTURE_DATE;
		const placeholder = process.env.OMP_CONFIGURE_REFRESH_PLACEHOLDER;

		// The previous run`s delivered OAuth entries and secret types, by the
		// env name each entry`s template action names.
		const previousEntries = new Map();
		const kept = previousFile(process.env.OMP_CONFIGURE_AUTH_PATH);
		try {
			for (const entry of Object.values(JSON.parse(kept ? kept.content : "{}") || {})) {
				// action is global, for the seed; a match here wants the name captured.
				const match = JSON.stringify(entry).match(new RegExp(action.source));
				if (match) previousEntries.set(match[1], entry);
			}
		} catch (err) {
			// A previous file that does not parse keeps nothing.
		}
		const previousTypes = new Map((previous.secrets || []).map((s) => [s && s.envName, s && s.type]));

		// oauthRefresh: the providers whose OAuth sign-in the control plane can
		// renew, and how. Each is a property of that provider`s public OAuth
		// client — its token endpoint, client id, and request encoding — not of
		// any one sign-in, so they are fixed here rather than read from the
		// store; they are the endpoints and clients omp`s own refresh uses. A
		// provider not listed is delivered as a plain token.
		const oauthRefresh = {
			// The Claude Pro/Max sign-in: Claude Code`s own client.
			anthropic: { tokenUrl: "https://platform.claude.com/v1/oauth/token", clientId: "9d1c250a-e61b-44d9-88ed-5944d1962f5e" },
			// The same client and endpoint the codex image refreshes as JSON.
			"openai-codex": { tokenUrl: "https://auth.openai.com/oauth/token", clientId: "app_EMoamEEZ73f0CkXaXp7hrann" },
			// xAI`s token endpoint takes the refresh request form-encoded.
			"xai-oauth": { tokenUrl: "https://auth.x.ai/oauth2/token", clientId: "b1a00492-073a-47ea-816f-4c329264a828", tokenRequestEncoding: "form" },
		};

		const envNames = new Set();
		const envNameFor = (provider) => {
			const base = "OMP_" + provider.toUpperCase().replace(/[^A-Z0-9]+/g, "_").replace(/^_+|_+$/g, "") + "_CREDENTIAL";
			let name = base;
			for (let n = 2; envNames.has(name); n++) name = `${base}_${n}`;
			envNames.add(name);
			return name;
		};

		const plan = [];
		const seen = new Set();
		for (const row of stored) {
			if (!row || typeof row.provider !== "string" || seen.has(row.provider)) continue;
			let value;
			try { value = JSON.parse(row.data); } catch (err) { continue; }
			if (!value || typeof value !== "object") continue;
			const provider = row.provider;
			// GitHub Copilot`s access token is minted from its GitHub token and
			// lives half an hour; the GitHub token is the credential, and it is
			// what omp refreshes with. So the secret is the refresh field, and
			// the entry carries the sentinel in both fields, already expired,
			// so omp mints a fresh access token through the proxy on first use
			// — the same shape the opencode image delivers Copilot in.
			const copilot = row.credential_type === "oauth" && provider === "github-copilot";
			const material = row.credential_type === "api_key" ? value.key : row.credential_type === "oauth" ? (copilot ? value.refresh : value.access) : undefined;
			if (typeof material !== "string" || !material) continue;
			seen.add(provider);
			const envName = envNameFor(provider);
			const sentinel = process.env["PREV_" + envName];
			if (sentinel && material === sentinel && previousEntries.has(envName)) {
				plan.push({ envName, provider, kept: true, type: previousTypes.get(envName) || "token", entry: previousEntries.get(envName) });
				continue;
			}
			// Dotted field access, never a quoted map key: this content is JSON,
			// so a quote inside a template action is escaped into the file
			// itself and Go`s template parser rejects it — which breaks every
			// sandbox launch while configure still reports success. An env name
			// is always a valid template field name, so there is nothing to
			// quote.
			const template = `{{ .secrets.${envName} }}`;
			const item = { envName, provider, kept: false };
			const identity = {};
			if (typeof value.accountId === "string" && value.accountId) identity.account_id = value.accountId;
			if (typeof value.email === "string" && value.email) identity.email = value.email;
			if (row.credential_type === "api_key") {
				item.type = "token";
				item.value = { token: value.key };
				item.apiKey = true;
			} else if (copilot) {
				item.type = "token";
				item.value = { token: value.refresh };
				item.entry = { access_token: template, refresh_token: template, expired: "1970-01-01T00:00:00.000Z", ...identity };
			} else {
				const spec = oauthRefresh[provider];
				if (spec && value.refresh && value.refresh !== value.access) {
					item.type = "oauth";
					item.value = { token: value.access, refreshToken: value.refresh, tokenUrl: spec.tokenUrl, clientId: spec.clientId };
					if (spec.tokenRequestEncoding) item.value.tokenRequestEncoding = spec.tokenRequestEncoding;
					if (Number(value.expires) > 0) item.value.accessTokenExpiresAt = Number(value.expires);
					// The refresh token stays in the control plane, so a refresh
					// from inside a sandbox could not succeed — and does not need
					// to, since a sentinel does not go stale.
					item.entry = { access_token: template, refresh_token: placeholder, expired: farFutureDate, ...identity };
				} else {
					item.type = "token";
					item.value = { token: value.access };
					item.entry = { access_token: template, refresh_token: placeholder, expired: farFutureDate, ...identity };
					const expires = Number(value.expires) || 0;
					if (expires > 0 && expires < farFuture) {
						item.notice = `The ${provider} sign-in expires ${new Date(expires).toISOString().slice(0, 10)}, and Discobox cannot renew it. Configure again after that, or sign in with an API key.`;
					}
				}
			}
			plan.push(item);
		}
		// API keys seeded into models.yml and not replaced by a new sign-in:
		// still pointed at their PREV_ variable, so kept.
		const models = readModels();
		if (models && models.providers && typeof models.providers === "object") {
			for (const [provider, spec] of Object.entries(models.providers)) {
				if (seen.has(provider) || !spec || typeof spec !== "object" || typeof spec.apiKey !== "string") continue;
				if (!/^PREV_OMP_[A-Z0-9_]+_CREDENTIAL$/.test(spec.apiKey)) continue;
				const envName = spec.apiKey.replace(/^PREV_/, "");
				if (!process.env[spec.apiKey] || envNames.has(envName)) continue;
				envNames.add(envName);
				seen.add(provider);
				plan.push({ envName, provider, kept: true, type: previousTypes.get(envName) || "token", apiKey: true });
			}
		}
		fs.writeFileSync(path.join(process.env.OMP_CONFIGURE_WORK, "plan.json"), JSON.stringify(plan));
		process.stdout.write(String(plan.length));
	'
}

# plan_labels prints the providers in the plan, comma-separated.
plan_labels() {
	node_env -e '
		const plan = JSON.parse(require("fs").readFileSync(require("path").join(process.env.OMP_CONFIGURE_WORK, "plan.json"), "utf8"));
		process.stdout.write(plan.map((p) => p.provider).join(", "));
	'
}

# verify_default_model runs a trivial prompt on the model a sandbox`s omp
# starts with — `omp --print` given no --model uses omp`s default: the
# `default` model role, else omp`s pick among what the signed-in providers
# offer.
#
# Only that model is checked, and a failure is a warning rather than a gate.
# Whether a request succeeds depends on the model as much as the credential —
# a plan that excludes it, a region it needs opting in to — and omp`s model
# list says nothing of either, so checking every provider on a model picked
# for it would report working credentials as broken. What a failure does show
# is that the harness`s first session would fail the same way, which is worth
# fixing while omp is one keystroke away.
#
# Stdin is closed: in print mode omp reads whatever is piped to it into the
# prompt, and this sandbox`s terminal is not the prompt.
verify_default_model() {
	set +e
	timeout 180 omp --print --no-session --no-title 'Reply with exactly: discobox-ok' </dev/null >"$WORK/verify.log" 2>&1
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
	node_env -e '
		const plan = JSON.parse(require("fs").readFileSync(require("path").join(process.env.OMP_CONFIGURE_WORK, "plan.json"), "utf8"));
		for (const item of plan) if (item.notice) process.stdout.write(item.notice + "\n");
	'
}

# confirm_launch holds the instructions on screen until the user is ready.
# omp draws a full-screen TUI, so anything printed right before launching it
# is gone before it can be read. Waiting for Enter means the banner is read
# while it is still the only thing on screen.
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
# the loop can only turn when a person asks it to: an attempt that fails
# without reaching the user — omp refusing to start, say — fails again the
# moment it is retried, and looping on that is a busy loop, not a retry. End of
# input means nobody is there to answer, which fails the configure flow rather
# than spinning.
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

# write_output records the result: one secret per provider, the file that
# delivers the OAuth sign-ins, models.yml naming each API key`s variable, and
# omp`s settings as the user left them. Secret shapes:
#   - kept:          usePrevious marker, no value.
#   - OAuth renewed: the access token plus refresh material, type oauth.
#   - anything else: a plain token.
write_output() {
	# sandbox-agent creates this directory for the sandbox user in config mode;
	# /run/discobox itself is root-owned and stays that way.
	mkdir -p "$(dirname "$OUTPUT")"
	node_env -e "$COMMON_JS"'
		const plan = readJSON(path.join(process.env.OMP_CONFIGURE_WORK, "plan.json"), []);
		const secrets = plan.map((item) => {
			const secret = { envName: item.envName, name: `omp: ${item.provider}`, type: item.type };
			if (item.kept) {
				secret.usePrevious = true;
			} else {
				secret.value = item.value;
			}
			return secret;
		});
		const files = [];
		const oauth = plan.filter((item) => item.entry);
		if (oauth.length > 0) {
			// The sentinel lands in the file, not the credential: the content is
			// a template, and each sandbox renders its own sentinels into it.
			const auth = {};
			for (const item of oauth) auth[item.provider] = item.entry;
			files.push({
				path: process.env.OMP_CONFIGURE_AUTH_PATH,
				template: true,
				content: JSON.stringify(auth, null, 2) + "\n",
			});
		}
		// models.yml: the user`s own, with Discobox`s entries — every API key`s
		// variable — put in fresh. Whatever PREV_ pointers the seed wrote and
		// whatever an earlier run named are dropped first, so a provider that
		// was signed out stays signed out. A file nobody here can read is
		// returned as it is, and the keys it would have carried are reported.
		const apiKeys = plan.filter((item) => item.apiKey);
		let models = readModels();
		if (models === undefined) {
			try {
				files.push({ path: process.env.OMP_CONFIGURE_MODELS_PATH, content: fs.readFileSync(process.env.OMP_CONFIGURE_MODELS_FILE, "utf8") });
			} catch (err) {}
			if (apiKeys.length > 0) {
				process.stderr.write(`models.yml could not be read as YAML, so the API keys for ${apiKeys.map((i) => i.provider).join(", ")} were not recorded in it. Fix the file and configure again.\n`);
			}
		} else {
			models = models || {};
			if (!models.providers || typeof models.providers !== "object") models.providers = {};
			for (const [provider, spec] of Object.entries(models.providers)) {
				if (spec && typeof spec === "object" && typeof spec.apiKey === "string" && ourVariable.test(spec.apiKey)) {
					delete spec.apiKey;
					if (Object.keys(spec).length === 0) delete models.providers[provider];
				}
			}
			for (const item of apiKeys) {
				const spec = models.providers[item.provider];
				models.providers[item.provider] = { ...(spec && typeof spec === "object" ? spec : {}), apiKey: item.envName };
			}
			if (Object.keys(models.providers).length === 0) delete models.providers;
			if (Object.keys(models).length > 0) {
				files.push({ path: process.env.OMP_CONFIGURE_MODELS_PATH, content: JSON.stringify(models, null, 2) + "\n" });
			}
		}
		// Settings are returned literally, as omp wrote them: they are the
		// user`s.
		for (const rel of process.env.OMP_CONFIGURE_SETTINGS_PATHS.split(" ").filter(Boolean)) {
			try {
				files.push({ path: rel, content: fs.readFileSync(path.join(process.env.OMP_CONFIGURE_HOME, rel), "utf8") });
			} catch (err) {
				// Not written in this session or any before it; nothing to keep.
			}
		}
		// Discobox`s settings, as they arrived — a judge model someone set by
		// editing the file is theirs.
		const settingsPath = process.env.OMP_CONFIGURE_SETTINGS_PATH;
		const settings = readJSON(path.join(process.env.OMP_CONFIGURE_HOME, settingsPath), {});
		if (typeof settings.judgeModel !== "string") settings.judgeModel = "";
		files.push({ path: settingsPath, content: JSON.stringify(settings, null, 2) + "\n" });
		fs.writeFileSync(process.env.OMP_CONFIGURE_OUTPUT, JSON.stringify({ files, secrets }));
	'
}

SEEDED_LABELS=""
seed_previous_credentials

while :; do
	printf '%s\n' "${C_BOLD}${C_WARN}========================================================${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN} Setting up omp — this is configuration, not a session${C_RESET}"
	printf '%s\n' "${C_BOLD}${C_WARN}========================================================${C_RESET}"
	echo
	echo "Discobox is about to start omp so you can set it up. This is a"
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
	echo "              as many as you like; omp's first-run setup offers the"
	echo "              same. This sandbox has no browser: a sign-in prints a"
	echo "              link to open on your own machine. ChatGPT's sign-in is"
	echo "              forwarded back in here; if Discobox warned its port is"
	echo "              taken, choose the headless (device) sign-in. Another"
	echo "              browser sign-in cannot reach back in here: paste the"
	echo "              code when omp asks for it, or use an API key."
	if [ -n "$SEEDED_LABELS" ]; then
		echo "              (Optional now: providers you already signed in to are kept.)"
	fi
	printf '%s\n' "  2. ${C_BOLD}${C_CMD}/exit${C_RESET}     Leave omp when you're done. Setup only finishes"
	echo "              once you exit — staying in blocks it."
	echo
	echo "Worth doing while you're in there:"
	echo
	printf '%s\n' "  ${C_CMD}/model${C_RESET}      Pick the model this harness starts with"
	printf '%s\n' "  ${C_CMD}/settings${C_RESET}   Thinking level, theme"
	echo
	confirm_launch

	# omp needs a real TTY for its interactive UI; run it under script. The
	# image keeps omp's first-run setup off (OMP_SKIP_SETUP), because a
	# sandbox's credentials arrive configured; here it is the sign-in, so it
	# is let run.
	set +e
	script -q -e -c 'env -u OMP_SKIP_SETUP omp' /dev/null
	omp_status=$?
	set -e
	# omp can leave its full-screen UI's contents behind when it exits, and
	# everything printed next would be drawn over them. Only after a clean
	# exit: when omp fails, what it printed is the explanation, and clearing
	# would erase it.
	if [ "$omp_status" -eq 0 ] && [ -t 1 ]; then
		printf '\033[?1049l\033[H\033[2J'
	fi
	echo

	count=$(classify_credentials)

	if [ "$count" -eq 0 ]; then
		if [ "$omp_status" -ne 0 ]; then
			# Not "the user signed in to nothing": omp refused to run at all,
			# and saying so points at the message it printed just above.
			printf '%s\n' "${C_ERR}${C_BOLD}omp exited with status $omp_status without a provider signed in.${C_RESET}" >&2
		else
			printf '%s\n' "${C_ERR}${C_BOLD}No provider is signed in.${C_RESET}" >&2
			echo "omp still runs without one — on a local model server, or a provider"
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
	echo "Your providers are still signed in. Start omp again to pick another"
	echo "model with /model, or sign in to the provider again with /login."
	if confirm_retry "saves everything as it is"; then
		continue
	fi
	break
done

print_notices
write_output

echo
echo "omp configuration complete."
