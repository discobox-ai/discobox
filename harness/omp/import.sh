#!/bin/sh
# discobox: import the OAuth credentials a harness file delivers into omp's
# own credential store.
#
# omp keeps credentials in a SQLite database (~/.omp/agent/agent.db) rather
# than a file a harness can deliver, and its importer, `omp auth-broker
# import`, is the supported way to add an OAuth credential to it. So the
# configure flow returns the sign-ins as .omp/agent/discobox-auth.json — one
# entry per provider, in the importer's shape, with the sentinel where the
# access token goes — and this script feeds each entry to the importer. The
# launcher runs it before every launch; the configure flow runs it to seed a
# reconfigure with the previous sign-ins.
#
# An entry whose access token is already the active credential is skipped:
# the importer appends rather than replaces, and a second copy of the same
# sentinel would only grow the store. When it differs — a first launch, a
# rotated secret, an account the user signed in to by hand inside the sandbox —
# the provider's rows are signed out and the delivered one imported, so the
# configured credential is the one omp uses, as the opencode image's auth.json
# replaces one connected by hand.
#
# Usage: omp-import-credentials <discobox-auth.json>
# Never fails the launch: a credential that cannot be imported is reported on
# stderr and the harness still starts.
set -eu

file=${1:?usage: omp-import-credentials <discobox-auth.json>}
[ -f "$file" ] || exit 0

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for provider in $(jq -r 'keys[]' "$file" 2>/dev/null); do
	# One entry per file: the importer takes the provider from --provider, so
	# the file's shape is the entry alone.
	jq --arg p "$provider" '.[$p]' "$file" >"$tmp/entry.json"
	delivered=$(jq -r '.access_token // ""' "$tmp/entry.json")
	[ -n "$delivered" ] || continue
	active=$(timeout 60 omp token "$provider" --raw 2>/dev/null | head -n 1) || active=""
	if [ "$active" = "$delivered" ]; then
		continue
	fi
	timeout 60 omp auth-broker logout "$provider" >/dev/null 2>&1 || true
	if ! timeout 60 omp auth-broker import "$tmp/entry.json" --provider "$provider" >/dev/null 2>&1; then
		printf '%s\n' "discobox: could not import the configured $provider credential into omp" >&2
	fi
done
