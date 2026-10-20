#!/bin/sh
# NSS reads the system trust store, so Chromium trusts the MITM CA. Chromium
# on Linux takes its roots from NSS alone and ignores /etc/ssl/certs, and NSS's
# built-in roots are Mozilla's list compiled into libnssckbi.so. Diverting that
# module to p11-kit's trust module makes it serve
# /etc/ssl/certs/ca-certificates.crt instead, which is the bundle
# discobox-ca-anchor appends the box's CA to at boot. So no per-user nssdb
# import is needed, and no boot step is added. The diversion keeps a libnss3
# upgrade from putting Mozilla's module back.
#
# Mozilla's module is diverted into a directory of its own, not beside itself:
# ldconfig, which every later package install runs, links a library's soname
# to the file that has it, and would point libnssckbi.so back at Mozilla's.
set -eu

# The package's own path: once diverted, dpkg -L also says where it went.
nssckbi="$(dpkg -L libnss3 | grep -x '/.*/libnssckbi\.so')"
aside="$(dirname "$nssckbi")/nss-mozilla/libnssckbi.so"
diverted="$(dpkg-divert --truename "$nssckbi")"
if [ "$diverted" != "$aside" ]; then
  # A diversion made beside the file, as images did before, is moved here:
  # the link is removed first, so taking the diversion away puts Mozilla's
  # module back in place, and the new one renames it aside.
  if [ "$diverted" != "$nssckbi" ]; then
    rm -f "$nssckbi"
    dpkg-divert --remove --rename "$nssckbi"
  fi
  mkdir -p "$(dirname "$aside")"
  dpkg-divert --add --rename --divert "$aside" "$nssckbi"
fi
ln -sfn pkcs11/p11-kit-trust.so "$nssckbi"
test -e "$(readlink -f "$nssckbi")"
