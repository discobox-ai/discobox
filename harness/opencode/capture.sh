#!/bin/sh
set -eu
exec node /usr/local/libexec/discobox/harness-capture/runtime.cjs opencode "$(dirname "$0")/opencode-image.json" "$@"
