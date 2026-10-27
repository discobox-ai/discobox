#!/bin/sh
set -eu
exec node /usr/local/libexec/discobox/harness-capture/runtime.cjs pi "$(dirname "$0")/pi-image.json" "$@"
