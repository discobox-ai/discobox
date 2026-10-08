#!/usr/bin/env bash
#---
# name: ShellCheck
# type: file
# pattern: "{**/*.sh,**/*.bash,**/*.bats,.envrc,sandbox-agent/image/*,vm-image/**}"
#---

# Thin trigger. Which files are shell scripts lives in scripts/shellcheck.sh,
# so this hook and `task check:shell` check the same set (ADR 0066 §1). The
# pattern can only name files, not shebangs: the extensionless scripts it
# reaches are the ones in the directories that hold them today, and one added
# elsewhere is still caught by `task check:shell` in CI.

set -euo pipefail

go tool task check:shell
