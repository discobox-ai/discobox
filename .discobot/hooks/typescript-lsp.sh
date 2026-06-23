#!/usr/bin/env bash
#---
# name: TypeScript LSP
# type: file
# engine: lsp
# pattern: "**/*.{ts,tsx,js,jsx}"
# ignore:
#   - ui/**
# language_id: typescript
# min_severity: warning
#---
set -euo pipefail
pnpm install --frozen-lockfile --ignore-scripts --silent >/dev/null
exec pnpm run -s lsp:typescript
