#!/bin/bash
#---
# name: Go mod tidy
# type: file
# pattern: "{go.mod,go.work,**/go.mod}"
# notify_llm: false
#---

# Thin trigger. The tidy rules live in scripts/go-mod-tidy.sh so `task tidy`
# and `task tidy:verify` run exactly what this hook runs (ADR 0066 §1).

set -euo pipefail

workspace="${DISCOBOT_WORKSPACE:-$(pwd)}"

# DISCOBOT_CHANGED_FILES is a space-separated list, so the split is deliberate;
# an empty value yields no arguments, which tidies every workspace module.
# shellcheck disable=SC2086
DISCOBOX_ROOT="$workspace" exec "$workspace/scripts/go-mod-tidy.sh" ${DISCOBOT_CHANGED_FILES:-}
