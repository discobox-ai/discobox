#!/bin/sh
# The docker feature's personalities of the agent (see cmd/discobox-sandbox-agent):
# - `runc`, on a directory prepended to containerd's and dockerd's PATH, so
#   every container either one starts has the MITM CA bundles and proxy env
#   injected into its OCI spec. See the shared runcca package.
# - `docker`, ahead of the real CLI on PATH, to give `docker build` the
#   pool-shared BuildKit cache. Non-build commands are exec'd straight through.
#   See the sandbox-agent/dockercache package.
set -eu

install -d -m 0755 /opt/discobox/bin
ln -sf /usr/local/bin/discobox-sandbox-agent /opt/discobox/bin/runc
ln -sf /usr/local/bin/discobox-sandbox-agent /usr/local/bin/docker
