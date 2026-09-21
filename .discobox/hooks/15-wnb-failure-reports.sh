#!/usr/bin/env bash
#---
# name: watchnbuild failure reports
# type: file
# pattern: "wnb-*-failed.txt"
#---

# Thin trigger. What counts as a failure report, and what is printed, lives in
# scripts/wnb-failure-reports.sh so this hook and `task check:dev-build` run
# exactly the same check (ADR 0066 §1).
#
# Failing here blocks the rest of the hook queue until a report is gone, and
# that is deliberate. A build report means the tree does not compile, so lint
# and tests behind it would fail anyway. A run report can be transient — the
# server retries through the ADR 0019 lock wait and a lost race for its socket
# (.wnb.yaml's retry block) — and it blocks the queue all the same, because a
# server that will not start is worth hearing about before anything else.
#
# The reports are deliberately not in .gitignore. The matcher drops every path
# Git would ignore before any hook pattern is considered, so ignoring them
# would mean this hook never runs. They are untracked instead, and that is a
# trade, not a free win: `git status` showing one is a useful sign that the dev
# loop is broken, but `git add -A` while it is broken commits it. The report is
# short-lived — wnb removes it as soon as the step works again — and this hook
# failing is itself the warning not to be committing yet.

set -euo pipefail

exec "${DISCOBOX_WORKSPACE:-$(pwd)}/scripts/wnb-failure-reports.sh"
