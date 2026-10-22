#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied.
# pi is a Node script (`#!/usr/bin/env node`, Node 22.19 or newer).
set -eu

# Provisioning also runs after boot, when ~/.npm may be a shared cache mount.
# Keep install scratch out of images without touching that mounted cache.
npm_config_cache=$(mktemp -d)
export npm_config_cache
trap 'rm -rf "$npm_config_cache"' EXIT

npm install -g @earendil-works/pi-coding-agent
