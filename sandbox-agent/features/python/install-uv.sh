#!/bin/bash
# uv, by its own installer, onto the system PATH.
set -euo pipefail

curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh
