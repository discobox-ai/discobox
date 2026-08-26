#!/bin/bash
#---
# name: Go format
# type: file
# pattern: "**/*.go"
# notify_llm: false
#---

# Thin trigger. The formatting rule lives in the Taskfile so this hook and
# `task fmt` cannot drift (ADR 0066 §1); the changed-file list is not passed
# on, because gofmt only rewrites what is not already clean and the whole
# tracked tree costs a couple of seconds.

set -euo pipefail

go tool task fmt
