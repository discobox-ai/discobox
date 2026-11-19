#!/bin/bash
#---
# name: Harness TypeScript
# type: file
# pattern: "harness/**/*.{ts,mts}"
#---

# Thin trigger: the rules live in package.json's scripts behind these targets
# (ADR 0066 §1). Format first, so the check reports what formatting cannot
# fix.

set -euo pipefail

go tool task fmt:ts check:ts
