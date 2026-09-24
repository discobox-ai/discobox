#!/bin/bash
#---
# name: Dockerfile dependency copies
# type: file
# pattern: "{**/*.go,go.mod,Dockerfile*,**/Dockerfile*}"
#---

set -euo pipefail

# Runs before the test builds, because it answers the same question in a second
# that a build stage takes minutes to reach: a new root-module package that an
# image's binaries import and no COPY brings into the build context. Nothing on
# a developer's machine notices, since the host has the whole repository.
#
# Triggered by Go files as well as Dockerfiles: the mistake is almost always a
# new import, in a module whose Dockerfile nobody thought to open.
go tool task check:dockerfile-deps
