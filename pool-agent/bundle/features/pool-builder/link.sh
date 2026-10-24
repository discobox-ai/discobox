#!/bin/sh
# The pool's builder and its registry run from this feature's directory, the
# bundle's stable link, where the image's binaries were (ADR 0044, ADR 0020):
# buildkitd and buildctl, BuildKit's runc and CNI plugins, the registry, and
# discobox-pool-runc as `runc` on the directory prepended to buildkitd's PATH.
set -eu

dir="${DISCOBOX_PROVISION_FEATURE_DIR:?}"
install -d -m 0755 /usr/local/bin /usr/bin /opt/discobox/bin /etc/discobox
for bin in buildkitd buildctl registry; do
  ln -sfn "$dir/bin/$bin" "/usr/local/bin/$bin"
done
for bin in buildkit-runc buildkit-cni-bridge buildkit-cni-firewall buildkit-cni-host-local buildkit-cni-loopback; do
  ln -sfn "$dir/bin/$bin" "/usr/bin/$bin"
done
ln -sfn "$dir/bin/discobox-pool-runc" /opt/discobox/bin/runc
