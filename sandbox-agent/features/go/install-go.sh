#!/bin/bash
# Go's latest release, unpinned: it tracks its own releases, and a new one is
# taken whenever this step is applied again.
set -euo pipefail

# Keep extraction and validation away from the published toolchain. The sibling
# directory also keeps publication on the same filesystem.
stage="$(mktemp -d /usr/local/.go-install.XXXXXXXX)"
# shellcheck disable=SC2329 # Invoked by the EXIT trap.
cleanup() {
    if [[ -e "$stage/previous" && ! -e /usr/local/go ]]; then
        mv "$stage/previous" /usr/local/go
    fi
    rm -rf "$stage"
}
trap cleanup EXIT

arch="$(dpkg --print-architecture)"
for attempt in 1 2 3; do
    echo "Go download attempt $attempt/3"
    # A failed HTTP transfer or tar extraction retries the entire download.
    # Do not reuse any files from an incomplete extraction.
    rm -rf "$stage/go"
    if version="$(curl -fsSL --connect-timeout 30 --max-time 60 'https://go.dev/VERSION?m=text' | head -1)" &&
        [[ "$version" =~ ^go[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] &&
        curl -fsSL --connect-timeout 30 --speed-limit 1 --speed-time 60 "https://go.dev/dl/${version}.linux-${arch}.tar.gz" | tar -C "$stage" -xz &&
        "$stage/go/bin/go" version; then
        if [[ -e /usr/local/go ]]; then
            mv /usr/local/go "$stage/previous"
        fi
        mv "$stage/go" /usr/local/go
        exit 0
    fi
    echo "Go download or validation failed (attempt $attempt/3)" >&2
    if (( attempt < 3 )); then
        sleep "$attempt"
    fi
done
echo "Go installation failed after 3 attempts; retry provisioning to try again" >&2
exit 1
