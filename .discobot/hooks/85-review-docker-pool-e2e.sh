#!/bin/bash
#---
# name: Docker pool flow e2e
# type: file
# pattern: "**/*.go"
# phase: review
#---

# Thin trigger. Which test, which package and which variable gates it live in
# the Taskfile so this hook and `task test:docker:pool-flow` cannot drift
# (ADR 0066 §1).

set -euo pipefail

go tool task test:docker:pool-flow
