#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied.
# pi is a Node script (`#!/usr/bin/env node`, Node 22.19 or newer).
# npm's cache is install-time scratch: left behind, it is half of what this
# step adds to an image.
set -eu

npm install -g @earendil-works/pi-coding-agent
rm -rf /root/.npm
