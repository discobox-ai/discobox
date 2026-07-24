#!/bin/sh
set -eu

mkdir -p /run/discobox
printf '%s\n' '{"secrets":[],"files":[]}' > /run/discobox/harness-configure.json
