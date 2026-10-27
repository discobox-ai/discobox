#!/usr/bin/env bash
# Retry-safe upload: retain an existing release's human-controlled state.
set -euo pipefail
version="${RELEASE_VERSION:?version required}"
artifacts="${1:-/artifacts}"
if ! gh release view "$version" >/dev/null 2>&1; then
  gh release create "$version" --verify-tag --title "$version" --generate-notes --prerelease
fi
gh release upload "$version" "$artifacts"/* --clobber
gh release view "$version" --json url --jq .url
