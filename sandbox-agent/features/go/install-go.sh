#!/bin/bash
# Go's latest release, unpinned: it tracks its own releases, and a new one is
# taken whenever this step is applied again.
set -euo pipefail

version="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)"
rm -rf /usr/local/go
curl -fsSL "https://go.dev/dl/${version}.linux-$(dpkg --print-architecture).tar.gz" | tar -C /usr/local -xz
/usr/local/go/bin/go version
