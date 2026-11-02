#!/bin/sh
# Which Node every Discobox script runs, recorded once, and made the one every
# user's PATH finds.
#
# The node feature adopts the box's own Node when its adopt check passes, and
# installs NodeSource's into /usr/bin when not; the runner tells an adopted
# feature's scripts which copy the check chose
# (DISCOBOX_PROVISION_ADOPTED_NODE and _NPM). Either way that copy is written
# to the record, /usr/local/lib/discobox/node.env, which the npm-global helper,
# the agent store and every harness's install step read instead of looking
# npm up on PATH (ADR 26-10-16-423 §4): on PATH it may be a copy the check
# refused, ahead of the chosen one. Then node, npm, npx and corepack are
# linked into /usr/local/bin from beside the chosen node, so a bare `node` in
# a launcher or a capture script runs it too, and what the box had there is
# moved aside (link-command).
#
# Run with `check` (check-node.sh), it changes nothing and exits non-zero when
# the record or a link is not what it would make: a box whose Node moved, or
# whose links were shadowed again, is put right at its next boot.
#
# DISCOBOX_NODE_ENV, DISCOBOX_NPM_PREFIX, DISCOBOX_INSTALLED_NODE_DIR and
# DISCOBOX_LINK_COMMAND move its fixed paths, for tests.
set -eu

record=${DISCOBOX_NODE_ENV:-/usr/local/lib/discobox/node.env}
prefix=${DISCOBOX_NPM_PREFIX:-/usr/local/lib/discobox/npm}
link_command=${DISCOBOX_LINK_COMMAND:-/usr/local/libexec/discobox/link-command}

if [ -n "${DISCOBOX_PROVISION_ADOPTED_NODE:-}" ]; then
  node=$DISCOBOX_PROVISION_ADOPTED_NODE
  npm=${DISCOBOX_PROVISION_ADOPTED_NPM:?the node feature adopts node and npm together}
else
  # Where install-node.sh's NodeSource package puts them.
  installed=${DISCOBOX_INSTALLED_NODE_DIR:-/usr/bin}
  node=$installed/node
  npm=$installed/npm
fi

contents="# The Node the node feature chose, adopted or installed: what Discobox's
# scripts run node and npm by, and the prefix every package it installs with
# npm goes into. Written by the node feature's select step; read, never probed.
DISCOBOX_NODE=$node
DISCOBOX_NPM=$npm
DISCOBOX_NPM_PREFIX=$prefix
"

if [ "${1-}" = check ]; then
  [ "$(cat "$record" 2>/dev/null)" = "$(printf '%s' "$contents")" ] || exit 1
  "$link_command" --check "$node" node
  "$link_command" --check "$npm" npm
  for command in npx corepack; do
    if [ -x "${node%/*}/$command" ]; then
      "$link_command" --check "${node%/*}/$command" "$command"
    fi
  done
  exit 0
fi

for command in "$node" "$npm"; do
  if [ ! -x "$command" ]; then
    echo "select-node: $command, the chosen Node's, is not there to run" >&2
    exit 1
  fi
done

mkdir -p "${record%/*}" "$prefix"
tmp=$(mktemp "$record.XXXXXX")
printf '%s' "$contents" >"$tmp"
chmod 0644 "$tmp"
mv -f "$tmp" "$record"

"$link_command" "$node" node
"$link_command" "$npm" npm
# Node 25 no longer ships corepack; link what is there.
for command in npx corepack; do
  if [ -x "${node%/*}/$command" ]; then
    "$link_command" "${node%/*}/$command" "$command"
  fi
done
