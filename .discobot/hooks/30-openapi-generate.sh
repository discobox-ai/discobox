#!/bin/bash
#---
# name: Regenerate OpenAPI
# type: file
# pattern: "{internal/api/**/*.go,internal/server/**/*.go,cmd/openapi/**/*.go}"
# notify_llm: false
#---

set -euo pipefail

go tool task generate
