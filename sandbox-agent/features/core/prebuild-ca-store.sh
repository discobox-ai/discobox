#!/bin/sh
# The complete system trust store, built ahead of boot so boot only adds the
# box's MITM CA to it rather than rebuilding every certificate to add one (see
# runcca.MaterializeTrustStore).
#
# Built at /opt rather than /etc/ssl/certs, and by a redirected copy of
# update-ca-certificates rather than the tool itself, because a build running
# inside a Discobox sandbox has /etc/ssl/certs bind-mounted over by the runc
# wrapper: writes there go to the mount and vanish when the layer commits,
# leaving the image with an empty store. LOCALCERTSDIR is pointed away so no
# build-time anchor is baked in; anchors belong to the sandbox that boots, and
# it adds its own.
set -eu

tool="$(mktemp)"
trap 'rm -f "$tool"' EXIT
mkdir -p /opt/discobox/ca-certificates
sed -e 's|^ETCCERTSDIR=.*|ETCCERTSDIR=/opt/discobox/ca-certificates|' \
    -e 's|^LOCALCERTSDIR=.*|LOCALCERTSDIR=/nonexistent|' \
    /usr/sbin/update-ca-certificates > "$tool"
chmod +x "$tool"
"$tool" --fresh
test -s /opt/discobox/ca-certificates/ca-certificates.crt
