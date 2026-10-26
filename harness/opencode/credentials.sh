#!/bin/sh
# opencode-credentials: puts the credentials the harness configuration delivers
# into opencode's own store (ADR 26-10-15-848 §3, §4).
#
# opencode 2 keeps credentials as rows in its SQLite database, which the
# configure flow cannot deliver as a file. What it delivers instead is
# opencode's own interchange format — `opencode auth export` output, with each
# provider's sentinel in place of its secret — as a templated harness file,
# and this script imports it through `opencode auth import`, opencode's
# supported write path.
#
#   opencode-credentials sync
#       Make the store hold the delivered credentials. Run by the launcher
#       before every launch, because the harness file is rewritten whenever a
#       terminal launches the harness and import never updates a credential it
#       already has.
#   opencode-credentials delivered [HOME]
#       Print the import entries the configuration delivered into HOME (this
#       user's when omitted): the export when there is one, else what a
#       configuration made under ADR 0127 — opencode 1's auth.json — becomes.
#       The judge imports these, and configure reads a previous run's files
#       through it.
#   opencode-credentials own
#       Read [{integrationID, value}] on stdin and print them as the image's
#       own import entries.
#
# The credentials the image delivers carry fixed ids, cred_discobox_<provider>,
# so they are told apart from a credential the user connected by hand inside a
# sandbox. This never logs out or changes one of those, but when it brings the
# store to a changed configuration it makes the delivered credential its
# provider's active one. This script is the one place that id is made.
set -eu

# Home-relative, as harness files are.
CREDENTIALS_PATH=.config/discobox/opencode-credentials.json
LEGACY_AUTH_PATH=.local/share/opencode/auth.json

# own stamps each entry as the image's: a fixed id per provider, active, so a
# credential it delivers takes over from whatever its provider used before.
own() {
	jq '[.[] | {
		id: ("cred_discobox_" + (.integrationID | gsub("[^A-Za-z0-9_-]"; "_"))),
		integrationID,
		label: "Discobox",
		active: true,
		value
	}]'
}

# from_auth_json is opencode's own legacy import, entry for entry (measured on
# 2.0.29): opencode 2 runs the same mapping when it first opens a database
# opencode 1 made beside an auth.json, and not for a database it creates
# fresh. An OAuth sign-in's method is ChatGPT's browser login for openai, a
# device code for github-copilot, opencode and xai, and opencode's generic
# "oauth" for any other. A wellknown entry's token becomes a key; the URL
# opencode fetched configuration from is not carried.
from_auth_json() {
	jq '
		[to_entries[]
		| select(.value | type == "object")
		| (.key | sub("/+$"; "")) as $id
		| select($id != "")
		| .value as $v
		| (if $v.type == "api" then
				{type: "key", key: $v.key} + (if $v.metadata then {metadata: $v.metadata} else {} end)
			elif $v.type == "wellknown" then
				{type: "key", key: $v.token}
			elif $v.type == "oauth" then
				{
					type: "oauth",
					methodID: (if $id == "openai" then "chatgpt-browser"
						elif ($id | IN("github-copilot", "opencode", "xai")) then "device"
						else "oauth" end),
					refresh: $v.refresh,
					access: $v.access,
					expires: $v.expires
				} + (if $v.accountId or $v.enterpriseUrl then
					{metadata: ((if $v.accountId then {accountID: $v.accountId} else {} end)
						+ (if $v.enterpriseUrl then {enterpriseUrl: $v.enterpriseUrl} else {} end))}
				else {} end)
			else empty end) as $value
		| {integrationID: $id, value: $value}]
	' | own
}

# delivered prints the import entries the configuration delivered into a home.
delivered() {
	home=${1:-$HOME}
	if [ -f "$home/$CREDENTIALS_PATH" ]; then
		jq -c 'if type == "array" then . else [] end' "$home/$CREDENTIALS_PATH"
	elif [ -f "$home/$LEGACY_AUTH_PATH" ]; then
		from_auth_json <"$home/$LEGACY_AUTH_PATH" | jq -c .
	else
		echo '[]'
	fi
}

# sync makes opencode's store hold exactly the delivered credentials among the
# ones the image owns. Every opencode command here starts a standalone server
# of its own, which took 10 to 40 seconds each in a busy sandbox, so it runs
# only the ones that change something:
#
#   - What it last synced is kept beside the store. While that is still what
#     is delivered and the database is still there, a launch runs nothing. A
#     credential removed by hand in the sandbox therefore stays removed until
#     the configuration changes.
#   - A database not created yet holds nothing, so a first launch only
#     imports.
#   - A credential that differs is logged out and imported again, since import
#     skips an id it already has.
#   - Import makes a credential active only for a provider that has no active
#     one, and logging out a provider's active credential promotes another. So
#     a delivered credential beside any other of its provider's — opencode's
#     own import of the same auth.json in a sandbox that ran opencode 1, or one
#     connected by hand — is switched to, and one that is the provider's only
#     credential is not.
sync_locked() {
	data="${XDG_DATA_HOME:-$HOME/.local/share}/opencode"
	synced="${XDG_STATE_HOME:-$HOME/.local/state}/discobox/opencode-credentials.synced"
	want=$(delivered)
	if [ -f "$data/opencode.db" ] && [ -f "$synced" ] && [ "$(cat "$synced")" = "$want" ]; then
		return 0
	fi
	if [ -f "$data/opencode.db" ]; then
		have=$(opencode auth export --standalone </dev/null 2>/dev/null)
	else
		have='[]'
	fi
	stale=$(jq -nr --argjson have "$have" --argjson want "$want" '
		$have[]
		| select(.id | startswith("cred_discobox_"))
		| . as $h
		| select(([$want[] | select(.id == $h.id and .value == $h.value)] | length) == 0)
		| "\(.integrationID)\t\(.id)"
	')
	# Every step's failure is counted rather than fatal, so one credential that
	# could not be changed does not stop the rest; any failure leaves what was
	# synced unrecorded, so the next launch tries again. The loops read
	# here-documents, not pipes, so they run in this shell and can count.
	tab=$(printf '\t')
	failed=0
	if [ -n "$stale" ]; then
		while IFS="$tab" read -r integration id; do
			opencode auth logout --standalone "$integration" "$id" </dev/null >/dev/null 2>&1 || failed=1
		done <<-EOF
			$stale
		EOF
	fi
	gone=$(printf '%s\n' "$stale" | jq -Rnc '[inputs | split("\t")[1] | select(. != null)]')
	missing=$(jq -nc --argjson have "$have" --argjson want "$want" --argjson gone "$gone" '
		[$want[] | . as $w
			| select(([$have[] | select(.id == $w.id and (.id | IN($gone[]) | not))] | length) == 0)]
	')
	if [ "$missing" != "[]" ]; then
		printf '%s\n' "$missing" | opencode auth import --standalone >/dev/null 2>&1 || failed=1
	fi
	# Every delivered credential not already active whose provider has
	# another credential: just imported beside one, or left inactive by a
	# switch that failed at an earlier launch. Whether that other one is
	# active is not asked: logging out a provider's active credential makes
	# opencode promote another (measured on 2.0.29), so one re-imported in
	# its place arrives inactive.
	switch=$(jq -nr --argjson have "$have" --argjson want "$want" --argjson gone "$gone" '
		[$have[] | select(.id | IN($gone[]) | not)] as $kept
		| $want[] | . as $w
		| select(any($kept[]; .id == $w.id and .active) | not)
		| select(any($kept[]; .integrationID == $w.integrationID and .id != $w.id))
		| "\(.integrationID)\t\(.id)"
	')
	if [ -n "$switch" ]; then
		while IFS="$tab" read -r integration id; do
			opencode auth switch --standalone "$integration" "$id" </dev/null >/dev/null 2>&1 || failed=1
		done <<-EOF
			$switch
		EOF
	fi
	if [ "$failed" -ne 0 ]; then
		rm -f "$synced"
		return 1
	fi
	mkdir -p "$(dirname "$synced")"
	printf '%s' "$want" >"$synced"
}

# The agent uses this same advisory lock around export, repair and handoff.
sync() {
	mkdir -p "$HOME/.config/discobox"
	(
		flock -x 9
		/usr/local/libexec/discobox/opencode-capture guard || exit 0
		sync_locked
	) 9>"$HOME/.config/discobox/harness-capture.lock"
}

case ${1-} in
sync) sync ;;
delivered) delivered "${2-}" ;;
own) own ;;
*)
	printf '%s\n' "usage: opencode-credentials sync | delivered [HOME] | own" >&2
	exit 2
	;;
esac
