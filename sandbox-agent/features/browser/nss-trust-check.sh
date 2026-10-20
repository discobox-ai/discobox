#!/bin/sh
# NSS's built-in roots are still p11-kit's trust module.
# The package's own path: once diverted, dpkg -L also says where it went.
nssckbi="$(dpkg -L libnss3 | grep -x '/.*/libnssckbi\.so')"
[ "$(readlink "$nssckbi")" = pkcs11/p11-kit-trust.so ]
