#!/bin/sh
# Has the box run the agent and proxy of this bundle and of the bootstrap
# placed beside it (pool-agent/poolunits): renders the sockets systemd holds
# for the pool, and restarts the running slot, or hands over to the other
# when the pool asks it to, once what they start with moved. This step's
# digest moves with the binary and the units (its inputs). A placement that
# moved alone is the maker's, which runs the same command after the apply.
#
# An apply refused because a slot still drains from the last handover exits
# 75 (bundle.UnitsDrainingExitCode). Run by the bundle's own apply, that is
# no failure: the maker runs this step again after every apply, with
# `report-draining`, and reads the 75 as "try later".
set -eu

[ -d /run/systemd/system ] || exit 0
code=0
/usr/local/bin/discobox-pool-agent units apply || code=$?
if [ "$code" -eq 75 ] && [ "${1:-}" != report-draining ]; then
	echo "units: a slot still drains from the last handover; the pool's maker applies the units again" >&2
	exit 0
fi
exit "$code"
