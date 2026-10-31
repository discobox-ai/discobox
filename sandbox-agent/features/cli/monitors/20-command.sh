#!/usr/bin/env bash
#---
# name: Command output
# description: Runs a command every interval and reports when its output changes
# options:
# - {name: cmd, required: true, description: "the command line, run with bash -c in the repository (home in a box with none)"}
# - {name: every, description: "how often to run it, a duration (30s, 5m); 60s when omitted"}
#---
# The image's command monitor (ADR 26-10-31-073 §9): `-o cmd='gh pr checks 412'`
# watches anything a command can print. The first run's output is the
# baseline; each change after it is one event, keyed so a change not yet
# popped is replaced by the next. The exit status is part of the output.
set -euo pipefail

cmd="${DISCOBOX_OPT_CMD:?}"
every="${DISCOBOX_OPT_EVERY:-60s}"
if [[ ! "$every" =~ ^[0-9]+[smh]$ ]]; then
  echo "every must be a number and s, m or h (30s, 5m), not '$every'" >&2
  exit 2
fi
state="${DISCOBOX_MONITOR_STATE:?}/output"

while :; do
  set +e
  output=$(bash -c "$cmd" 2>&1)
  code=$?
  set -e
  current="$output"$'\n'"[exit $code]"
  if [[ -f "$state" ]] && [[ "$current" != "$(<"$state")" ]]; then
    first=$(grep -m1 -E '\S' <<<"$output" || true)
    jq -nc --arg cmd "$cmd" --arg first "$first" --arg body "$current" --argjson code "$code" \
      '{kind: "command.changed", key: "output",
        title: "output of `\($cmd | .[0:80])` changed: \($first | .[0:120])",
        summary: "exit \($code)", body: $body}'
  fi
  printf '%s' "$current" >"$state"
  sleep "$every"
done
