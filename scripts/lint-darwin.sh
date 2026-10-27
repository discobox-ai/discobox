#!/usr/bin/env bash
# Lint, on darwin, the packages whose files are not the ones Linux lints.
#
# CI's check job lints every package on Linux. Most packages compile from the
# same files on darwin, so linting them again on the macOS runner repeats that
# work on the slowest runner CI has. What only a Mac can lint is the packages
# darwin builds from different files: build-tagged and _darwin.go files, and cgo
# against the macOS SDK. `go list` names each package's files under both GOOSes
# and the packages whose lists differ are linted, so a new darwin file is picked
# up without a list here to keep current.
#
# cgo is on for both listings so that only the GOOS changes between them; a
# cross listing would otherwise drop every cgo file and flag the whole tree.
#
# Usage: scripts/lint-darwin.sh <golangci-lint binary> [golangci-lint flags...]
# Called by `go tool task native:macos`.

set -euo pipefail

if [ "$(go env GOOS)" != darwin ]; then
	echo "lint-darwin: lints darwin's own files, so it runs on darwin" >&2
	exit 1
fi

lint="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"
shift

cd "$(git rev-parse --show-toplevel)"

files='{{.Dir}} {{.GoFiles}} {{.CgoFiles}} {{.TestGoFiles}} {{.XTestGoFiles}}'

# Every module go.work uses, each linted from its own directory with its own
# packages, as `go tool task check` does.
go list -m -f '{{.Dir}}' | while IFS= read -r module; do
	dirs="$(cd "$module" && comm -13 \
		<(GOOS=linux CGO_ENABLED=1 go list -e -f "$files" ./... | sort) \
		<(GOOS=darwin CGO_ENABLED=1 go list -e -f "$files" ./... | sort) |
		awk -v module="$module" '{ print "." substr($1, length(module) + 1) }')"
	if [ -z "$dirs" ]; then
		continue
	fi
	# Word-split on purpose: one package directory per argument.
	# shellcheck disable=SC2086
	echo "lint-darwin: .${module#"$PWD"}:" $dirs
	# shellcheck disable=SC2086
	(cd "$module" && "$lint" run "$@" $dirs)
done
