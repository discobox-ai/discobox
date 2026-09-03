#!/usr/bin/env bash
#---
# name: Go LSP
# type: file
# engine: lsp
# pattern: "**/*.go"
# ignore:
#   - "**/*_darwin.go"
# language_id: go
# min_severity: warning
#---
# Darwin files are ignored because this server cannot type-check them off a
# Mac. Opening one makes gopls build a darwin view, and a cross-compiled darwin
# view is CGO_ENABLED=0, so Code-Hex/vz — an entirely cgo package that the vz
# provider's darwin files reach through — exports nothing. Every symbol it
# defines is then reported undefined, on code that compiles fine on the machine
# that runs it. The darwin CI job is what checks these files.
set -euo pipefail

exec go tool gopls serve
