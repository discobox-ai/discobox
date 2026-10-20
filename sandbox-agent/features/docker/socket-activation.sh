#!/bin/sh
# The daemon starts when something first connects to its socket, not at boot:
# Docker's packages enable docker.service as they install, and a sandbox's
# daemon waits to be used.
set -eu

systemctl disable docker.service containerd.service
systemctl enable docker.socket
mkdir -p /var/lib/docker
