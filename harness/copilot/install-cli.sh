#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied:
# into Discobox's npm prefix with the npm the node feature chose, and its
# commands linked into /usr/local/bin (npm-global), never npm's default global
# prefix, which on some boxes is one user's nvm.
# The npm package is a loader; the CLI itself is the platform package npm
# installs beside it as an optional dependency. On first run the CLI unpacks
# itself under ~/.cache/copilot/pkg, which the base layer declares as per-user
# pool cache, so a pool pays that once per version rather than once per
# sandbox.
set -eu

exec /usr/local/libexec/discobox/npm-global install @github/copilot
