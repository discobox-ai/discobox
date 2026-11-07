#!/bin/sh
set -eu
test "$(readlink /opt/discobox/bin/runc)" = /opt/discobox/current/features/core/discobox-sandbox-agent
test -x /opt/discobox/bin/runc
