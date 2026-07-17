#!/bin/bash
#---
# name: Mermaid diagram validation
# type: file
# pattern: "**/*.md"
#---

set -euo pipefail

# Browser detection lives in the check:mermaid task, so it applies whether the
# validation runs from this hook or from the task directly. A caller that
# exports PUPPETEER_EXECUTABLE_PATH still overrides it.
go tool task check:mermaid
