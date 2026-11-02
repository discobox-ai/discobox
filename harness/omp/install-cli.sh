#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied:
# into Discobox's npm prefix with the npm the node feature chose, and its
# commands linked into /usr/local/bin (npm-global), never npm's default global
# prefix, which on some boxes is one user's nvm.
# omp is a Bun script (`#!/usr/bin/env bun`) and runs on the bun feature's
# Bun; its native addon is an optional dependency npm picks for the platform.
#
# One of its dependencies, onnxruntime-node, downloads onnxruntime's CUDA
# build from GitHub on install on linux-x64 -- hundreds of megabytes for a GPU
# a sandbox does not have, and a download that has hung a build outright.
# ONNXRUNTIME_NODE_INSTALL_CUDA=skip is its own switch for that; the
# manifest's env sets it too, for the version store's installs.
set -eu

export ONNXRUNTIME_NODE_INSTALL_CUDA=skip
exec /usr/local/libexec/discobox/npm-global install @oh-my-pi/pi-coding-agent
