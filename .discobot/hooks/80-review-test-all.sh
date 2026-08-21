#!/bin/bash
#---
# name: Full Go test suite
# type: file
# pattern: "**/*.{go,mod,sum}"
#---

set -euo pipefail

# ci:test, not test:all: seeding a project needs an inspectable harness image,
# and a checkout that has never run `task build:images` has none, so every test
# that creates a sandbox fails for a reason that has nothing to do with the
# change under review. ci:test builds label-only stand-ins first (ADR 0066 §7).
# Nothing in this suite runs a container, so the stand-ins cost no fidelity.
go tool task ci:test
