#!/bin/sh
set -eu
# Initialization updates the same credential-bearing local state as handoff.
# Take the agent/native-import lock across its entire read/modify/write.
if [ "${1-}" = init ]; then
    mkdir -p "$HOME/.config/discobox"
    exec flock -x "$HOME/.config/discobox/harness-capture.lock" node /usr/local/libexec/discobox/harness-capture/runtime.cjs claude-code "$(dirname "$0")/claude-code-image.json" "$@"
fi
exec node /usr/local/libexec/discobox/harness-capture/runtime.cjs claude-code "$(dirname "$0")/claude-code-image.json" "$@"
