#!/bin/bash
#---
# name: Go format
# type: file
# pattern: "**/*.go"
# notify_llm: false
#---

set -euo pipefail

if [ -n "${DISCOBOT_CHANGED_FILES:-}" ]; then
  gofmt -w $DISCOBOT_CHANGED_FILES
fi
