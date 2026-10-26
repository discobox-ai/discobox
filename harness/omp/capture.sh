#!/bin/sh
set -eu
exec node /usr/local/libexec/discobox/harness-capture/runtime.cjs omp "$(dirname "$0")/omp-image.json" "$@"
