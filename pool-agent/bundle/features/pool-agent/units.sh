#!/bin/sh
# Has the box run the agent and proxy of this bundle and of the bootstrap
# placed beside it (pool-agent/poolunits): renders the sockets systemd holds
# for the pool, and restarts the running slot, or hands over to the other
# when the pool asks it to, once what they start with moved. This step's
# digest moves with the binary and the units (its inputs). A placement that
# moved alone is the maker's, which runs the same command after the apply.
set -eu

[ -d /run/systemd/system ] || exit 0
exec /usr/local/bin/discobox-pool-agent units apply
