#!/bin/sh
# npm's cache is install-time scratch; left behind it put 100MB in every image,
# and each harness repeats this to install its own CLI.
set -eu

npm install -g pnpm
rm -rf /root/.npm
