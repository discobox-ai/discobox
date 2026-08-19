#!/usr/bin/env bash
#---
# name: Go LSP
# type: file
# engine: lsp
# pattern: "**/*.go"
# ignore:
#   - "endpoint/iroh_supported.go"
#   - "endpoint/iroh_transport_test.go"
# language_id: go
# min_severity: warning
#---
set -euo pipefail

# gopls analyzes one build configuration, and this one is the default: the
# files behind the `iroh` tag are therefore invisible to it and are listed in
# `ignore` above rather than reported as unanalyzable on every run. They are
# not unchecked — `task check:iroh` and `task test:iroh` compile, lint, and
# test that configuration.
exec go tool gopls serve
