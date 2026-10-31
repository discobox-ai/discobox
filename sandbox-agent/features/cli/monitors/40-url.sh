#!/usr/bin/env bash
#---
# name: URL
# description: Reports when a URL's status or body changes
# options:
# - {name: url, required: true, description: the URL to fetch}
# - {name: every, description: "how often to fetch it, a duration (30s, 5m); 60s when omitted"}
#---
# The image's url monitor (ADR 26-10-31-073 §9). The first fetch is the
# baseline; each change of status, or of the body's digest, after it is one
# event, keyed so a change not yet popped is replaced by the next.
set -euo pipefail

url="${DISCOBOX_OPT_URL:?}"
every="${DISCOBOX_OPT_EVERY:-60s}"
if [[ ! "$every" =~ ^[0-9]+[smh]$ ]]; then
  echo "every must be a number and s, m or h (30s, 5m), not '$every'" >&2
  exit 2
fi
state="${DISCOBOX_MONITOR_STATE:?}/last"
body=$(mktemp)
err=$(mktemp)
trap 'rm -f "$body" "$err"' EXIT

while :; do
  # The status is one word — the HTTP code, or `error` with curl's reason on
  # stderr — since it is read back as the first field of the state line.
  if ! status=$(curl -sS -L -o "$body" -w '%{http_code}' --max-time 30 "$url" 2>"$err"); then
    cat "$err" >&2
    status=error
    : >"$body"
  fi
  current="$status $(sha256sum "$body" | cut -d' ' -f1)"
  if [[ -f "$state" ]]; then
    read -r was_status was_digest <"$state"
    now_digest="${current##* }"
    if [[ "$status" != "$was_status" ]]; then
      jq -nc --arg url "$url" --arg s "$status" --arg was "$was_status" \
        '{kind: "url.status", key: "change", title: "\($url) now answers \($s) (was \($was))", url: $url}'
    elif [[ "$now_digest" != "$was_digest" ]]; then
      jq -nc --arg url "$url" --rawfile b "$body" \
        '{kind: "url.changed", key: "change", title: "\($url) changed", url: $url, body: ($b | .[0:65536])}'
    fi
  fi
  echo "$current" >"$state"
  sleep "$every"
done
