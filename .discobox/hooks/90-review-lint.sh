#!/bin/bash
#---
# name: GolangCI-Lint
# type: file
# pattern: "**/*.{go,mod,sum}"
#---

set -euo pipefail

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

status=0
go tool task check 2>&1 | tee "$out" || status=$?

# A processor failure is only a warning to golangci-lint, but it makes it emit
# the *unfiltered* result set: `exclusions` (including `generated: lax`) stops
# applying and thousands of issues in generated code surface as real findings.
# The usual trigger is a stale cache entry pointing at a deleted worktree file.
# Fail loudly rather than reporting an untrustworthy issue list.
if grep -q "Can't process results by .* processor" "$out"; then
	cat >&2 <<'EOF'

=======================================================================
golangci-lint could not apply its exclusion config.

Any issues listed above are UNFILTERED and must not be treated as real
findings -- exclusions such as `generated: lax` were silently skipped.

This is usually a stale lint cache. Fix with:

    go tool golangci-lint cache clean

then re-run this hook.
=======================================================================
EOF
	exit 1
fi

exit "$status"
