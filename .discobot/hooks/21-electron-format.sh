#!/bin/bash
#---
# name: Electron format
# type: file
# pattern: "electron/**/*.{ts,mjs,js,json,yaml,yml,md}"
# notify_llm: false
#---

set -euo pipefail

files=()
for file in ${DISCOBOT_CHANGED_FILES:-}; do
  if [[ "$file" == electron/* && -f "$file" ]]; then
    files+=("$PWD/$file")
  fi
done

if [ ${#files[@]} -gt 0 ]; then
  pnpm --dir electron exec prettier --write "${files[@]}"
fi
