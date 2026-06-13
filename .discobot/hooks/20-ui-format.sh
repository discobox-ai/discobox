#!/bin/bash
#---
# name: UI format
# type: file
# pattern: "ui/**/*.{svelte,ts,js,json,css,md,yaml,yml}"
# notify_llm: false
#---

set -euo pipefail

files=()
for file in ${DISCOBOT_CHANGED_FILES:-}; do
  if [[ "$file" == ui/* && -f "$file" ]]; then
    files+=("$PWD/$file")
  fi
done

if [ ${#files[@]} -gt 0 ]; then
  pnpm --dir ui exec prettier --write "${files[@]}"
fi
