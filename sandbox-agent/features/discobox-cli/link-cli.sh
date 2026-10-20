#!/bin/sh
# Run from the platform tree, where the build put it, as the agent is.
set -eu

ln -sfn "${DISCOBOX_PROVISION_FEATURE_DIR:?}/discobox" /usr/local/bin/discobox
