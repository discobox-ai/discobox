#!/usr/bin/env bash
# Decide whether a CI run has code to check: CI's `changes` job runs it, and
# every heavy job waits on its answer (.github/workflows/ci.yml).
#
# Usage: scripts/ci-changes.sh <event> [<base> <head>]
#
# Only a pull request is filtered: any other event (a push to main) has code.
# A pull request has code unless every path it changes is under docs/. Its
# changes are the diff from <base> to <head>, by default HEAD^1 to HEAD, which
# for the merge commit actions/checkout checks out on a pull request is exactly
# what the PR does to its base branch. Renames are split into a delete and an
# add, so a file moved into docs/ still counts its old path.
#
# Prints code=true or code=false, and appends it to $GITHUB_OUTPUT when set.
set -euo pipefail

event=${1:?usage: scripts/ci-changes.sh <event> [<base> <head>]}
base=${2:-HEAD^1}
head=${3:-HEAD}

code=true
if [ "$event" = pull_request ]; then
  changed=$(git diff --no-renames --name-only "$base" "$head")
  printf 'Changed paths:\n%s\n' "${changed:-(none)}" >&2
  # An empty diff has nothing outside docs/ either, but nothing to skip for:
  # it is a PR GitHub would not normally let anyone open, so it runs everything.
  if [ -n "$changed" ] && ! grep -qv '^docs/' <<<"$changed"; then
    code=false
  fi
fi

echo "code=$code"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "code=$code" >>"$GITHUB_OUTPUT"
fi
