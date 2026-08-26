#!/bin/bash
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

workspace="${DISCOBOT_WORKSPACE:-$(pwd)}"

# DISCOBOT_CHANGED_FILES is a space-separated list, so the split is deliberate;
# an empty value yields no arguments, which builds every tracked Dockerfile.
# shellcheck disable=SC2086
DISCOBOX_ROOT="$workspace" exec "$workspace/scripts/dockerfile-test-builds.sh" ${DISCOBOT_CHANGED_FILES:-}
