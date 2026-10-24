#!/bin/sh
# The pool agent runs from its feature's directory, the bundle's stable link,
# so a new bundle needs no step applied again: the link follows the tree.
set -eu

dir="${DISCOBOX_PROVISION_FEATURE_DIR:?}"
install -d -m 0755 /usr/local/bin /etc/discobox/pool-agent /var/lib/discobox/proxy
ln -sfn "$dir/discobox-pool-agent" /usr/local/bin/discobox-pool-agent
