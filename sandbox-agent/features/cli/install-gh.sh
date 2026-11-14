#!/bin/sh
# gh, from GitHub's own apt repository. Its signing keys are this feature's own
# copy (githubcli-archive-keyring.asc), not one fetched beside the packages they
# sign: the same keys, by fingerprint, the sandbox base image's
# GH_APT_KEY_FINGERPRINTS pins.
set -eu
export DEBIAN_FRONTEND=noninteractive

install -m 0755 -d /etc/apt/keyrings
install -m 0644 githubcli-archive-keyring.asc /etc/apt/keyrings/githubcli-archive-keyring.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.asc] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list
apt-get update
apt-get install -y --no-install-recommends gh
