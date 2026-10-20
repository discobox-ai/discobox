#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied.
# npm's cache is install-time scratch: left behind, it is half of what this
# step adds to an image.
set -eu

npm install -g @anthropic-ai/claude-code
rm -rf /root/.npm
