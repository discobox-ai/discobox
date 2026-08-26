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

workspace="${DISCOBOX_WORKSPACE:-$(pwd)}"

# DISCOBOX_CHANGED_FILES is newline-separated (the hook runner joins the paths
# with "\n"), so splitting on newlines alone keeps paths containing spaces
# intact. An empty value yields no arguments, which tidies every workspace module.
mapfile -t changed < <(printf '%s' "${DISCOBOX_CHANGED_FILES:-}")

DISCOBOX_ROOT="$workspace" exec "$workspace/scripts/go-mod-tidy.sh" "${changed[@]}"
