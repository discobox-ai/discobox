#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied.
# The npm package is a loader; the CLI itself is the platform package npm
# installs beside it as an optional dependency. On first run the CLI unpacks
# itself under ~/.cache/copilot/pkg, which the base layer declares as per-user
# pool cache, so a pool pays that once per version rather than once per
# sandbox.
# npm's cache is install-time scratch: left behind, it is half of what this
# step adds to an image.
set -eu

npm install -g @github/copilot
rm -rf /root/.npm
