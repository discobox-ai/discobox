#!/bin/bash
#---
# name: Discobox Electron
# description: Runs the Discobox Electron desktop shell against the UI dev server
# order: 30
#---

set -euo pipefail

export DISCOBOX_UI_DEV_URL="${DISCOBOX_UI_DEV_URL:-http://127.0.0.1:5173}"
export LIBGL_ALWAYS_SOFTWARE="${LIBGL_ALWAYS_SOFTWARE:-1}"
export GALLIUM_DRIVER="${GALLIUM_DRIVER:-llvmpipe}"
export MESA_LOADER_DRIVER_OVERRIDE="${MESA_LOADER_DRIVER_OVERRIDE:-llvmpipe}"

for _ in {1..60}; do
  if (echo > /dev/tcp/127.0.0.1/5173) >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

exec go tool task dev:electron:shell
