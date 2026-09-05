#!/usr/bin/env bash
#---
# name: Dockerfile test builds
# type: file
# pattern: "{Dockerfile*,**/Dockerfile*}"
#---

# Thin trigger. Which image is built how — its context, its build arguments, and
# what it has to be built after — lives in each image's own Taskfile target, and
# scripts/dockerfile-test-builds.sh dispatches to them, so this hook and
# `task check:dockerfile-builds` run exactly the same builds (ADR 0066 §1).

set -euo pipefail

workspace="${DISCOBOX_WORKSPACE:-$(pwd)}"

# DISCOBOX_CHANGED_FILES is newline-separated (the hook runner joins the paths
# with "\n"), so splitting on newlines alone keeps paths containing spaces
# intact. mapfile is bash 4, and macOS's /bin/bash is 3.2, so the shebang above
# asks PATH for a bash rather than naming that one.
# An empty value yields no arguments, which builds every tracked Dockerfile.
mapfile -t changed < <(printf '%s' "${DISCOBOX_CHANGED_FILES:-}")

DISCOBOX_ROOT="$workspace" exec "$workspace/scripts/dockerfile-test-builds.sh" "${changed[@]}"
