#!/bin/bash
#---
# name: Mermaid diagram validation
# type: file
# pattern: "**/*.md"
#---

set -euo pipefail

if [[ -z "${PUPPETEER_EXECUTABLE_PATH:-}" ]]; then
	for browser in chromium chromium-browser google-chrome google-chrome-stable; do
		if path="$(command -v "$browser")"; then
			export PUPPETEER_EXECUTABLE_PATH="$path"
			break
		fi
	done
fi

go tool task check:mermaid
