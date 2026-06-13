#!/bin/bash
#---
# name: Electron checks
# type: file
# pattern: "electron/**/*.{ts,mjs,js,json,yaml,yml,md}"
# description: Run Electron formatting and TypeScript checks before review completes
# phase: review
#---

set -euo pipefail

go tool task check:electron
