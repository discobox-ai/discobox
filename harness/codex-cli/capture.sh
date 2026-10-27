#!/bin/sh
set -eu
exec node /usr/local/libexec/discobox/harness-capture/runtime.cjs codex "$(dirname "$0")/codex-image.json" "$@"
