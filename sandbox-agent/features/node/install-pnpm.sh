#!/bin/sh
# pnpm, for every user of the box, into Discobox's npm prefix (npm-global).
set -eu

exec /usr/local/libexec/discobox/npm-global install pnpm
