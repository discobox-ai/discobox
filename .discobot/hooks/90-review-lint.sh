#!/bin/bash
#---
# name: GolangCI-Lint
# type: file
# pattern: "**/*.go"
#---

set -euo pipefail

go tool task check
