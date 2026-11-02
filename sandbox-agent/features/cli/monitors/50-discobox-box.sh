#!/bin/sh
#---
# name: Discobox boxes
# description: Other discoboxes' state, their primary terminal going busy or idle, and their secret requests
# interval: 30s
# min-interval: 10s
# options:
# - {name: id, description: "box ids to watch, comma-separated"}
# - {name: tag, description: "watch the boxes whose tags match: key or key=value, space-separated, every one matching"}
# - {name: created-by, description: "watch the boxes this box created (me) or another box created (its id)"}
# - {name: watch, description: "state, terminal, requests, comma-separated; all three when omitted"}
# - {name: interval, description: "how long each run follows the event stream before the next reconnects, and how often requests are listed; 30s when omitted, at least 10s"}
#---
# The image's discobox-box monitor (ADR 26-11-01-560), written in Go and run
# by the agent (sandbox-agent/monitors/discoboxbox). It follows the
# project's sandbox event stream through the pool's gate, so it needs a box
# allowed to call the discobox API (discoboxApi); without that, each run
# fails saying so.
exec discobox-sandbox-agent monitor-builtin discobox-box
