#!/bin/sh
#---
# name: Discobox boxes
# description: Other discoboxes' state, their primary terminal going busy or idle, and their secret requests
# interval: 30s
# min-interval: 10s
# options:
# - {name: id, description: "box ids to watch, comma-separated"}
# - {name: tag, description: "watch the boxes whose tags match: key or key=value, space-separated, every one matching"}
# - {name: watch, description: "state, terminal, requests, comma-separated; all three when omitted"}
# - {name: interval, description: "how often to poll, a duration; 30s when omitted, at least 10s"}
#---
# The image's discobox-box monitor, written in Go and run by the agent
# (sandbox-agent/monitors/discoboxbox). It reads the discobox API through the
# pool's gate, so it needs a box allowed to call it (discoboxApi); without
# that, each run fails saying so.
exec discobox-sandbox-agent monitor-builtin discobox-box
