#!/bin/sh
set -eu
exec node /usr/local/libexec/discobox/harness-capture/runtime.cjs copilot "$(dirname "$0")/copilot-image.json" "$@"
