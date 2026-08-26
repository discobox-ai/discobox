#!/usr/bin/env bash
#---
# name: Go LSP
# type: file
# engine: lsp
# pattern: "**/*.go"
# language_id: go
# min_severity: warning
#---
set -euo pipefail

exec go tool gopls serve
