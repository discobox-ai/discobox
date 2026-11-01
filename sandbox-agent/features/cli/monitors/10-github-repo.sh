#!/bin/sh
#---
# name: GitHub repository
# description: New activity on a repository's issues, pull requests and Actions runs
# interval: 60s
# min-interval: 10s
# since: true
# options:
# - {name: repo, required: true, description: owner/name}
# - {name: watch, description: "issues, prs, actions, comma-separated; all three when omitted and no ids or filters are given"}
# - {name: issue, description: "issue numbers to watch, comma-separated"}
# - {name: pr, description: "pull request numbers to watch, comma-separated"}
# - {name: head, description: "watch the PRs whose branch matches this glob (discobox/*), with their checks and conflicts"}
# - {name: author, description: "watch the PRs this login opened, with their checks and conflicts"}
# - {name: run, description: "Actions run ids to watch, comma-separated"}
# - {name: branch, description: "only the Actions runs of this branch (main)"}
# - {name: interval, description: "how often to poll, a duration; 60s when omitted, at least 10s"}
#---
# The image's github-repo monitor (ADR 26-10-31-073 §9), written in Go and
# run by the agent (sandbox-agent/monitors/githubrepo): a quiet repository
# costs no rate limit, and a catch-up after a long sleep keeps its progress.
# Given DISCOBOX_MONITOR_SINCE (`test --since`, ADR 26-11-01-375 §3), its
# first run starts from that time instead of now.
exec discobox-sandbox-agent monitor-builtin github-repo
