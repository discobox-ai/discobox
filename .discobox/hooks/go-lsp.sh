#!/usr/bin/env bash
#---
# name: Go LSP
# type: file
# engine: lsp
# pattern: "**/*.go"
# ignore:
#   - "server/providers/vz/internal/vzvm/{vm,host}_{darwin,other}.go"
# language_id: go
# min_severity: warning
#---
# vzvm is split on `darwin && cgo` / `!darwin || !cgo`, because Code-Hex/vz is
# an entirely cgo package. Opening either half makes gopls build a darwin view,
# and that view does not resolve the cgo tag the way the go command does: it
# takes both halves as one package and reports every symbol as declared twice.
# The split is correct — `go vet` type-checks darwin with cgo on and off, and
# the darwin CI job compiles the real one — so those four files are not
# diagnosed here. Everything else in the package, and every other _darwin.go
# file in the tree, still is.
set -euo pipefail

exec go tool gopls serve
