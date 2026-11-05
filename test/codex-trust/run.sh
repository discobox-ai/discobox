#!/bin/sh
# Pin the vendor binary used by the trust contract test; never use PATH's Codex.
set -eu
root=$(CDPATH='' cd -- "$(dirname "$0")/../.." && pwd)
version=$(cat "$root/test/codex-trust/version")
prefix="$root/.tmp/codex-trust"
npm install --prefix "$prefix" --no-audit --no-fund "@openai/codex@$version"
# A private mount namespace isolates /etc/codex and the installed harness files
# from both this developer box and other test cases. CI's Linux runner has sudo.
exec sudo -n unshare --mount --fork python3 "$root/test/codex-trust/run.py" "$root" "$prefix/node_modules/.bin/codex" "$version"
