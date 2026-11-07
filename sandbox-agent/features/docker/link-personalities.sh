#!/bin/sh
# The docker feature's personalities of the agent (see cmd/discobox-sandbox-agent):
# - `runc`, on a directory prepended to containerd's and dockerd's PATH, so
#   every container either one starts has the MITM CA bundles and proxy env
#   injected into its OCI spec. See the shared runcca package.
# - `docker`, ahead of the real CLI on PATH, to give `docker build` the
#   pool-shared BuildKit cache. Non-build commands are exec'd straight through.
#   See the sandbox-agent/dockercache package. Linked by core's link-command,
#   which moves aside what the box had at the name.
#
# With `check` (check-personalities.sh) it changes nothing and fails when a
# link is gone or shadowed, so the next boot links it again.
set -eu

if [ "${1-}" = check ]; then
  [ "$(readlink /opt/discobox/bin/runc 2>/dev/null)" = /usr/local/bin/discobox-sandbox-agent ]
  exec /usr/local/libexec/discobox/link-command --check /usr/local/bin/discobox-sandbox-agent docker
fi
install -d -m 0755 /opt/discobox/bin
ln -sf /usr/local/bin/discobox-sandbox-agent /opt/discobox/bin/runc
exec /usr/local/libexec/discobox/link-command /usr/local/bin/discobox-sandbox-agent docker
