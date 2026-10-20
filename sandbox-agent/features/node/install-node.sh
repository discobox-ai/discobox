#!/bin/bash
# Node from NodeSource's apt repository, which tracks Node's own releases.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

curl -fsSL https://deb.nodesource.com/setup_25.x | bash -
sed -i 's|http://|https://|g' /etc/apt/sources.list.d/nodesource.list 2>/dev/null || true
apt-get install -y --no-install-recommends nodejs
