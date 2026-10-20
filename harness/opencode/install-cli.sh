#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied.
# opencode 2 (@opencode/cli), ADR 26-10-15-848.
# npm's cache is install-time scratch: left behind, it is half of what this
# step adds to an image.
set -eu

npm install -g @opencode/cli
rm -rf /root/.npm
