#!/usr/bin/env bash
#---
# name: Tailwind CSS LSP
# type: file
# engine: lsp
# pattern: "ui/**/*.svelte"
# language_id: svelte
# min_severity: warning
#---
set -euo pipefail
pnpm install --frozen-lockfile --ignore-scripts --silent >/dev/null
exec pnpm run -s lsp:tailwindcss
