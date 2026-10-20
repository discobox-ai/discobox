#!/bin/sh
# The harness's CLI, from npm, at its latest release when this step is applied.
# omp is a Bun script (`#!/usr/bin/env bun`) and runs on the bun feature's
# Bun; its native addon is an optional dependency npm picks for the platform.
#
# One of its dependencies, onnxruntime-node, downloads onnxruntime's CUDA
# build from GitHub on install on linux-x64 -- hundreds of megabytes for a GPU
# a sandbox does not have, and a download that has hung a build outright.
# ONNXRUNTIME_NODE_INSTALL_CUDA=skip is its own switch for that; the
# manifest's env sets it too, for the version store's installs.
# npm's cache is install-time scratch: left behind, it is half of what this
# step adds to an image.
set -eu

ONNXRUNTIME_NODE_INSTALL_CUDA=skip npm install -g @oh-my-pi/pi-coding-agent
rm -rf /root/.npm
