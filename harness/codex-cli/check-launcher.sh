#!/bin/sh
# The launcher is still this harness's: core lays a no-op at the same path,
# and core applied again after this feature would leave it there.
exec cmp -s launch.sh /usr/local/bin/discobox-harness-run
