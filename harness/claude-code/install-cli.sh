#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied:
# into Discobox's npm prefix with the npm the node feature chose, and its
# commands linked into /usr/local/bin (npm-global), never npm's default global
# prefix, which on some boxes is one user's nvm.
set -eu

exec /usr/local/libexec/discobox/npm-global install @anthropic-ai/claude-code
