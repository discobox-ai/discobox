#!/usr/bin/env bash
#---
# name: File lines
# description: Lines appended to a file from now on, every one or those matching a pattern
# options:
# - {name: path, required: true, description: "the file, absolute or relative to the repository (home in a box with none)"}
# - {name: match, description: "an extended regular expression a line must match; every line when omitted"}
#---
# The image's file monitor (ADR 26-10-31-073 §9). It starts at the file's
# end, so only lines written after it is enabled are events, and follows the
# file through rotation and recreation.
set -euo pipefail

path="${DISCOBOX_OPT_PATH:?}"
match="${DISCOBOX_OPT_MATCH:-}"
tail -n0 -F "$path" 2>/dev/null \
  | grep --line-buffered -E -- "${match:-.}" \
  | jq --unbuffered -Rc --arg path "$path" '{kind: "file.line", title: .[0:200], summary: $path, body: (if length > 200 then . else "" end)}'
