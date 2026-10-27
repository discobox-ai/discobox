#!/usr/bin/env bash
# Install only a versioned CLI archive whose digest is reviewed in this repo.
set -euo pipefail
version=1.0.0-beta.21
arch=$(go env GOARCH)
case "$arch" in amd64|arm64) ;; *) echo "unsupported CLI architecture: $arch" >&2; exit 1 ;; esac
archive="dagger_v${version}_linux_${arch}.tar.gz"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl --fail --silent --show-error --location --retry 3 \
  "https://dl.dagger.io/dagger/releases/$version/$archive" -o "$work/$archive"
(cd "$work" && sha256sum --check --ignore-missing /pipeline/dagger-cli-checksums.txt)
tar -xzf "$work/$archive" -C "$work" dagger
install -m 0755 "$work/dagger" /usr/local/bin/dagger
