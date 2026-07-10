#!/bin/bash
#---
# name: GolangCI-Lint
# type: file
# pattern: "**/*.{go,mod,sum}"
#---

set -euo pipefail

go tool task check
