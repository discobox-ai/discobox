#!/bin/sh
# The mark and wordmark the desktop viewer's page is drawn with, served as
# /brand/ by `discobox-sandbox-agent desktop`. The repository's own brand
# directory, which the build put beside this script, linked rather than copied
# so a new tree brings its own.
set -eu

install -d -m 0755 /usr/local/share/discobox
ln -sfn "${DISCOBOX_PROVISION_FEATURE_DIR:?}/brand" /usr/local/share/discobox/brand
