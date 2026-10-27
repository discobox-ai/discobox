#!/usr/bin/env bash
# Dockerfile source and Go-import coverage, run inside the Dagger workspace.
set -euo pipefail

set -eu
missing=""
# dockerfile:context. A COPY source is relative to the build context,
# not to the Dockerfile, and the harness bundles are the ones built from
# somewhere other than the repository root.
for entry in \
  server/providers/dockerworker/sandbox-image/Dockerfile:server/providers/dockerworker/sandbox-image \
  pool-agent/Dockerfile:. \
  sandbox-agent/Dockerfile:. \
  vm-image/Dockerfile:. \
  vm-image/kernel/Dockerfile:. \
  harness/claude-code/Dockerfile:harness/claude-code \
  harness/codex-cli/Dockerfile:harness/codex-cli \
  harness/opencode/Dockerfile:harness/opencode \
  harness/copilot/Dockerfile:harness/copilot \
  harness/omp/Dockerfile:harness/omp \
  harness/pi/Dockerfile:harness/pi \
  harness/shell/Dockerfile:harness/shell; do
  dockerfile="${entry%:*}"
  context="${entry##*:}"
  # All but the last token of a COPY are sources; --from copies from an
  # earlier stage rather than the context, so it is not ours to check.
  # Collected rather than reported inside a pipeline: a `while` fed by
  # one runs in a subshell, where a non-zero exit goes nowhere.
  # COPY shell-form sources are whitespace-separated words.
  # shellcheck disable=SC2013
  for src in $(awk '/^COPY / && !/--from=/ {
                      for (i = 2; i < NF; i++) if ($i !~ /^--/) print $i
                    }' "$dockerfile"); do
    if [ ! -e "$context/$src" ]; then
      missing="$missing$dockerfile: COPY $src: no such path in build context $context
"
    fi
  done
done
if [ -n "$missing" ]; then
  printf '%s' "$missing" >&2
  exit 1
fi

set -eu
missing=""
for entry in pool-agent/Dockerfile:build:pool-agent sandbox-agent/Dockerfile:sandbox-agent-builder:sandbox-agent sandbox-agent/Dockerfile:sandbox-agent-builder:access sandbox-agent/Dockerfile:discobox-cli-builder:cli; do
  dockerfile="${entry%%:*}"
  module="${entry##*:}"
  stage="${entry#*:}"
  stage="${stage%:*}"
  # Only what the stage that builds the module copies: a package copied
  # into another stage of the same Dockerfile is not in this one's
  # build context, and counting it hid a missing COPY.
  copied="$(awk -v stage="$stage" '
    /^FROM / { current = ""; for (i = 2; i < NF; i++) if (toupper($i) == "AS") current = $(i + 1) }
    /^COPY [a-z]/ && current == stage { split($2, parts, "/"); print (parts[1] == "pkg" ? parts[1] "/" parts[2] : parts[1]) }' "$dockerfile" | sort -u)"
  if [ -z "$copied" ]; then
    missing="$missing$dockerfile: no stage named $stage copies anything; the stage that builds $module was renamed
"
    continue
  fi
  for dep in $(cd "$module" && go list -deps ./... |
                 sed -n 's|^github.com/discobox-ai/discobox/||p' |
                 awk -F/ '{ print ($1 == "pkg" ? $1 "/" $2 : $1) }' | sort -u); do
    # A here-string avoids grep -q closing a producer pipe under pipefail.
    if ! grep -qx "$dep" <<<"$copied"; then
      missing="$missing$dockerfile: $module imports $dep, which nothing copies into the build context
"
    fi
  done
done
if [ -n "$missing" ]; then
  printf '%s' "$missing" >&2
  exit 1
fi
