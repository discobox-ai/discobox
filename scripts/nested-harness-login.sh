# shellcheck shell=sh
# Sourced by nested-claude-auth.sh and nested-codex-auth.sh, never run.
#
# Gives a nested development server's harness config a login the outer sandbox
# already holds, through the same API a person uses by hand: a project secret
# per credential, routed where the harness's bundle declares it goes, bound to
# the harness config under its variable, and the harness's files saved into
# the config's overlay. Every box on the harness then receives it, exactly as
# it would after a sign-in in one box whose harness change was saved
# (ADR 26-11-02-152). There is no configure flow to drive.
#
# The caller sets HARNESS (the slug), PROJECT and a die function, and passes
# apply_nested_login a JSON document {secrets: [{envName, name, value}],
# files: [{path, content}]}. Re-running it updates the secrets in place and
# replaces the files it names, leaving every other saved file alone.

# The API is called directly: only a local socket is supported, since a nested
# development server is always one.
nested_login_socket() {
	SERVER=${DISCOBOX_SERVER:-$(discobox admin --help 2>&1 | sed -n 's/.*--server string.*(default "\([^"]*\)").*/\1/p')}
	case "$SERVER" in
	unix://*) SOCKET=${SERVER#unix://} ;;
	*) die "only a unix:// server is supported, got '$SERVER'" ;;
	esac
	[ -S "$SOCKET" ] || die "no server socket at $SOCKET; is the nested server running (go tool task dev)?"
}

# api METHOD PATH [BODY] prints the body and the status on its last line.
api() {
	if [ $# -ge 3 ]; then
		printf '%s' "$3" | curl -sS --unix-socket "$SOCKET" -X "$1" -H 'Content-Type: application/json' \
			--data-binary @- -w '\n%{http_code}' "http://discobox.local/projects/$PROJECT$2"
	else
		curl -sS --unix-socket "$SOCKET" -X "$1" -w '\n%{http_code}' "http://discobox.local/projects/$PROJECT$2"
	fi
}

# api_ok runs api and prints the body, failing unless the status is 2xx. A
# failure names the request but never echoes the body sent, which can hold a
# credential.
api_ok() {
	response=$(api "$@")
	status=$(printf '%s' "$response" | tail -n1)
	body=$(printf '%s' "$response" | sed '$d')
	case "$status" in
	2??) printf '%s' "$body" ;;
	*) die "$1 $2: HTTP $status: $body" ;;
	esac
}

apply_nested_login() {
	output=$1
	nested_login_socket
	harness_id=$(discobox admin harness ls -o json | jq -er --arg slug "$HARNESS" '.harnessConfigs[] | select(.slug == $slug) | .id') ||
		die "the server has no $HARNESS harness"
	config=$(api_ok GET "/harness-configs/$harness_id") || exit 1

	count=$(printf '%s' "$output" | jq '.secrets | length')
	i=0
	while [ "$i" -lt "$count" ]; do
		secret=$(printf '%s' "$output" | jq -c ".secrets[$i]")
		env=$(printf '%s' "$secret" | jq -r .envName)
		# Named after the harness and its variable, so a rerun finds it again.
		name=$(printf '%s-%s' "$HARNESS" "$env" | tr '[:upper:]_' '[:lower:]-')
		# Where the bundle says the harness sends this credential; a variable
		# it declares no routes for goes anywhere, as a save would route it.
		routes=$(printf '%s' "$config" | jq -c --arg env "$env" \
			'[.secrets[]? | select(.name == $env) | .routes // empty][0] // [{hosts: ["*"]}]')
		existing=$(api_ok GET "/secrets" | jq -r --arg name "$name" '.secrets[]? | select(.name == $name) | .id' | head -n1)
		if [ -n "$existing" ]; then
			api_ok PUT "/secrets/$existing" "$(printf '%s' "$secret" | jq -c --argjson routes "$routes" '{value, routes: $routes}')" >/dev/null
			secret_id=$existing
		else
			secret_id=$(api_ok POST "/secrets" "$(printf '%s' "$secret" | jq -c --arg name "$name" --argjson routes "$routes" \
				'{name: $name, type: "token", value, routes: $routes}')" | jq -er .id) || exit 1
		fi
		bound=$(api_ok GET "/secret-bindings?targetKind=harnessConfig&target=$harness_id" |
			jq -r --arg id "$secret_id" --arg env "$env" '[.secretBindings[]? | select(.secretId == $id and .envVar == $env and .use)] | length')
		if [ "$bound" = 0 ]; then
			api_ok POST "/secret-bindings" "$(jq -nc --arg id "$secret_id" --arg target "$harness_id" --arg env "$env" \
				'{secret: $id, targetKind: "harnessConfig", target: $target, envVar: $env, use: true}')" >/dev/null
		fi
		i=$((i + 1))
	done

	# The files join the saved overlay, replacing only the paths they name, at
	# the revision just read: a save that lands in between is a conflict, not
	# something to overwrite.
	files=$(printf '%s' "$config" | jq -c --argjson add "$(printf '%s' "$output" | jq -c '.files')" '
		($add | map(.path)) as $paths
		| [(.configuredFiles // [])[] | select(.path as $p | $paths | index($p) | not) | {path, content}] + $add')
	revision=$(printf '%s' "$config" | jq -er .revision) || die "the $HARNESS harness reports no revision"
	api_ok PATCH "/harness-configs/$harness_id" "$(jq -nc --argjson files "$files" --argjson revision "$revision" \
		'{revision: $revision, configuredFiles: $files}')" >/dev/null
}
