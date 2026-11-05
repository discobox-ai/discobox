#!/bin/sh
set -eu
[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload
