#!/bin/sh
# A new agent binary under the stable link reaches what runs it only once it
# restarts: this step's digest moves with the binary (its inputs), so a new
# bundle restarts the agent and its proxy, and the same one restarts nothing.
# The first run finds neither running, and leaves their start to the units
# step after it.
set -eu

[ -d /run/systemd/system ] || exit 0
systemctl try-restart discobox-pool-agent.service discobox-proxy.service
