#!/bin/sh
set -eu

# Provisioning also runs after boot, when ~/.npm may be a shared cache mount.
# Keep install scratch out of images without touching that mounted cache.
npm_config_cache=$(mktemp -d)
export npm_config_cache
trap 'rm -rf "$npm_config_cache"' EXIT

npm install -g pnpm
