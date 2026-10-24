#!/bin/sh
# New binaries under the stable links reach what runs them only once it
# restarts: this step's digest moves with them (its inputs), so a new bundle
# restarts the builder, the registry and the mediator, and the same one
# restarts nothing.
set -eu

[ -d /run/systemd/system ] || exit 0
systemctl try-restart discobox-buildkitd.service discobox-registry.service discobox-buildkit-mediator.service
