#!/bin/sh
# The agent and discobox-access run from the platform tree, where the build put
# them (ADR 26-10-16-423 §7), so a new agent needs no step applied again: the
# links follow the tree. discobox-access is its own binary, never a
# personality: the credential CLI is meant to run outside a discobox (ADR 0031).
#
# Then core's personalities of the agent, each a link it dispatches on by
# argv[0] (see cmd/discobox-sandbox-agent): `discobox-power` (keepalive, sleep
# and wake, ADR 0108 §4), `discobox-ca-anchor` (discobox-trust-ca.service
# runs it from /opt/discobox/current under that name, since it starts before
# core is provisioned), `discobox-hook-publish` (the harnesses' hooks) and `discobox-provision` (a
# feature's placeholders, and waiting on one). The docker feature lays its own.
set -eu

dir="${DISCOBOX_PROVISION_FEATURE_DIR:?}"
install -d -m 0755 /usr/local/bin
ln -sfn "$dir/discobox-sandbox-agent" /usr/local/bin/discobox-sandbox-agent
ln -sfn "$dir/discobox-access" /usr/local/bin/discobox-access
for c in discobox-power discobox-ca-anchor discobox-hook-publish discobox-provision; do
  ln -sf /usr/local/bin/discobox-sandbox-agent "/usr/local/bin/$c"
done
