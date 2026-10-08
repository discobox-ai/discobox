#!/bin/sh
# Store a boxd (boxd.sh) API key as an exchange secret: the server trades the
# key for boxd's hour-long token, and a discobox granted it holds only that
# token, in BOXD_TOKEN (ADR 26-10-08-452).
#
# Usage: scripts/boxd-secret.sh [--key-file FILE] [--name NAME] [discobox flags...]
#
#   scripts/boxd-secret.sh --key-file ./key --server http://127.0.0.1:8080
#   BOXD_API_KEY=bxd_... scripts/boxd-secret.sh
#   scripts/boxd-secret.sh            # asks for the key, without echoing it
#
# The key is read from --key-file, else $BOXD_API_KEY, else the terminal. It is
# piped to the CLI on stdin, never put on a command line. Flags the script does
# not know (--server, --project, ...) go to every discobox call; $DISCOBOX
# names the CLI (default: discobox on PATH).
#
# A secret of that name already there is updated in place — the recipe and the
# key together, which is the only way the server takes a new recipe — so its
# grants survive a key rotation. A key boxd refuses is never stored.
set -eu

name=boxd
key_file=
discobox=${DISCOBOX:-discobox}
while [ $# -gt 0 ]; do
	case $1 in
	--key-file) key_file=$2; shift 2 ;;
	--key-file=*) key_file=${1#*=}; shift ;;
	--name) name=$2; shift 2 ;;
	--name=*) name=${1#*=}; shift ;;
	-h|--help) sed -n '2,20p' "$0"; exit 0 ;;
	*) break ;;
	esac
done

# The token goes to boxd.sh and the hosts beneath it — the gRPC API at
# boxd.sh:9443 — and the exchange is at app.boxd.sh, inside that binding.
recipe='{"url":"https://app.boxd.sh/api/v1/auth/token","fields":["api_key"],"body":{"api_key":"{api_key}"},"tokenPath":"token","expiresAtPath":"expires_at"}'

if [ -n "$key_file" ]; then
	key=$(tr -d ' \r\n' < "$key_file")
elif [ -n "${BOXD_API_KEY:-}" ]; then
	key=$BOXD_API_KEY
elif [ -t 0 ]; then
	printf 'boxd API key (from boxd auth keys create): ' >&2
	stty -echo
	trap 'stty echo' EXIT
	IFS= read -r key
	stty echo
	trap - EXIT
	printf '\n' >&2
else
	echo "no key: give --key-file, set BOXD_API_KEY, or run on a terminal" >&2
	exit 2
fi
case $key in
bxd_*) ;;
*) echo "that is not a boxd API key (they start bxd_)" >&2; exit 2 ;;
esac

if "$discobox" "$@" secret get "$name" >/dev/null 2>&1; then
	printf %s "$key" | "$discobox" "$@" secret update "$name" \
		--exchange-recipe "$recipe" --field api_key=-
else
	printf %s "$key" | "$discobox" "$@" secret create --name "$name" --type exchange --host boxd.sh \
		--exchange-recipe "$recipe" --field api_key=-
fi
