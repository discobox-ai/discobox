#!/bin/bash
#---
# name: GolangCI-Lint
# type: file
# pattern: "{**/*.{go,mod,sum},ui/**/*.{svelte,ts,js,json,css,md,yaml,yml},electron/**/*.{ts,mjs,js,json,yaml,yml,md}}"
#---

set -euo pipefail

go tool task check
