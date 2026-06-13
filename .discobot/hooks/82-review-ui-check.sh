#!/bin/bash
#---
# name: UI checks
# type: file
# pattern: "ui/**/*.{svelte,ts,js,json,css,md,yaml,yml}"
# description: Run UI formatting checks, linting, type checks, tests, and audit before review completes
# phase: review
#---

set -euo pipefail

go tool task check:ui
