#!/bin/sh
# Where the agent keeps its state and the sandbox's bootstrap lands.
set -eu

mkdir -p /etc/discobox /run/discobox/agent-terminals /var/lib/discobox /workspace
