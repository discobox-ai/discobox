#!/bin/sh
# Run from the platform tree, where the build put it, as the agent is, and
# linked onto PATH by core's link-command, which moves aside what the box had
# at the name. With `check` (check-review.sh) it changes nothing and fails when
# the link is gone or shadowed, so the next boot links it again.
set -eu

check=
if [ "${1-}" = check ]; then
  check=--check
fi
exec /usr/local/libexec/discobox/link-command $check "${DISCOBOX_PROVISION_FEATURE_DIR:?}/discobox-review" discobox-review
