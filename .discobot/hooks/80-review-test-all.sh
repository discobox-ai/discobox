#!/bin/bash
#---
# name: Full Go test suite
# type: file
# pattern: "**/*.{go,mod,sum}"
#---

set -euo pipefail

go tool task test:all
