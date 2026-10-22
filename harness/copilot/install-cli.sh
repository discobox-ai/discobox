#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied.
# The npm package is a loader; the CLI itself is the platform package npm
# installs beside it as an optional dependency. On first run the CLI unpacks
# itself under ~/.cache/copilot/pkg, which the base layer declares as per-user
# pool cache, so a pool pays that once per version rather than once per
# sandbox.
set -eu

# Provisioning also runs after boot, when ~/.npm may be a shared cache mount.
# Keep install scratch out of images without touching that mounted cache.
npm_config_cache=$(mktemp -d)
export npm_config_cache
trap 'rm -rf "$npm_config_cache"' EXIT

npm install -g @github/copilot
