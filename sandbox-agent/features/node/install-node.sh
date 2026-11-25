#!/bin/bash
# Node from NodeSource's apt repository, which tracks Node's own releases: the
# package the Docker sandbox base builds in, installed here on an image without
# it. Its signing key is this feature's own copy (nodesource-repo.asc), not one
# fetched beside the packages it signs: the key, by fingerprint, the sandbox
# base image's NODESOURCE_APT_KEY_FINGERPRINT pins. The source and preference
# are at the paths NodeSource's setup script writes them, so where it set a box
# up they are replaced; the source an older setup script wrote
# (nodesource.list) and the keyring it fetched are removed, since apt refuses
# two sources for one repository signed by different keys.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

line=25
rm -f /etc/apt/sources.list.d/nodesource.list /usr/share/keyrings/nodesource.gpg
install -m 0755 -d /etc/apt/keyrings
install -m 0644 nodesource-repo.asc /etc/apt/keyrings/nodesource.asc
printf 'Types: deb\nURIs: https://deb.nodesource.com/node_%s.x\nSuites: nodistro\nComponents: main\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/nodesource.asc\n' \
  "$line" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/nodesource.sources
printf 'Package: nodejs\nPin: origin deb.nodesource.com\nPin-Priority: 600\n' > /etc/apt/preferences.d/nodejs
apt-get update
apt-get install -y --no-install-recommends nodejs
