#!/bin/bash
set -euo pipefail

if ! rustup --version >/dev/null 2>&1; then
  curl --proto "=https" --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain none
fi
