#!/bin/sh
set -eu
exec node /usr/local/libexec/discobox/harness-capture/runtime.cjs claude-code "$(dirname "$0")/claude-code-image.json" "$@"
