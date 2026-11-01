#!/bin/sh
# Set up a boxd pool on a discobox server: make a new boxd API key with the
# boxd CLI, create (or update) a vm provider named boxd on the boxd driver with
# it, and create a pool named boxd on that provider.
#
# Usage: scripts/boxd-pool.sh [--name NAME] [--base ID] [discobox flags...]
#
#   scripts/boxd-pool.sh                                  # provider and pool "boxd"
#   scripts/boxd-pool.sh --server http://127.0.0.1:8080
#   scripts/boxd-pool.sh --base <boxd image or snapshot ID>
#
# Run `boxd login` first: boxd makes API keys only for an interactive session.
# Each run makes a new key, named <name>-<random>. When the provider already
# exists its config is replaced with the new key, and the key it held is left in
# boxd for you to revoke. Flags the script does not know (--server, --project,
# ...) go to every discobox call; $DISCOBOX names the CLI (default: discobox on
# PATH) and $BOXD the boxd CLI (default: boxd on PATH).
#
# The provider has no controlPlaneUrl, so the server opens a channel to its pool
# machine and needs no public URL (ADR 26-10-30-875). It sets deliverProviderKey,
# because boxd issues a pool no narrower key from an API key (ADR 26-10-23-551
# §4): the key is placed on the pool's machine, and nothing expires it. The
# server seals the key in the provider's row and never returns it (ADR
# 26-10-31-694).
set -eu

name=boxd
base=
discobox=${DISCOBOX:-discobox}
boxd=${BOXD:-boxd}
# The script's flags may come anywhere; the rest are rotated to the back of
# "$@", in order, for discobox.
n=$#
while [ "$n" -gt 0 ]; do
	arg=$1; shift; n=$((n - 1))
	case $arg in
	--name|--base)
		[ "$n" -gt 0 ] || { echo "$arg needs a value" >&2; exit 2; }
		val=$1; shift; n=$((n - 1))
		if [ "$arg" = --name ]; then name=$val; else base=$val; fi ;;
	--name=*) name=${arg#*=} ;;
	--base=*) base=${arg#*=} ;;
	-h|--help) sed -n '2,25p' "$0"; exit 0 ;;
	*) set -- "$@" "$arg" ;;
	esac
done

for cli in "$boxd" "$discobox"; do
	command -v "$cli" >/dev/null 2>&1 || { echo "$cli is not on PATH" >&2; exit 2; }
done
command -v jq >/dev/null 2>&1 || { echo "jq is not on PATH" >&2; exit 2; }

suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
key_name="$name-$suffix"
if ! out=$("$boxd" auth keys create "$key_name" 2>&1); then
	printf '%s\n' "$out" >&2
	echo "boxd did not make a key; run \`boxd login\` and try again" >&2
	exit 1
fi
key=$(printf '%s\n' "$out" | grep -oE 'bxd_[A-Za-z0-9_-]+' | head -1 || true)
if [ -z "$key" ]; then
	echo "boxd made key $key_name but printed no bxd_ key; revoke it in boxd" >&2
	exit 1
fi
echo "made boxd API key $key_name" >&2

# The config goes to the CLI as @file, never on a command line, where other
# users' ps would read the key.
config=$(mktemp)
trap 'rm -f "$config"' EXIT
chmod 600 "$config"
jq -n --arg key "$key" --arg base "$base" \
	'{driver: "boxd", apiKey: $key, deliverProviderKey: true} + (if $base == "" then {} else {base: $base} end)' >"$config"

if "$discobox" "$@" admin provider get "$name" >/dev/null 2>&1; then
	"$discobox" "$@" admin provider update "$name" --config "@$config" >/dev/null
	echo "updated provider $name with the new key; revoke the key it held before in boxd" >&2
else
	"$discobox" "$@" admin provider create --type vm --name "$name" --config "@$config" >/dev/null
	echo "created provider $name" >&2
fi

if "$discobox" "$@" admin pool get "$name" >/dev/null 2>&1; then
	echo "pool $name already exists" >&2
else
	"$discobox" "$@" admin pool create "$name" --provider "$name" >/dev/null
	echo "created pool $name; watch it come up with: $discobox admin pool get $name" >&2
fi
echo "run a discobox on it with: $discobox run --pool $name" >&2
