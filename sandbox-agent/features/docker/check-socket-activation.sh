#!/bin/sh
# A failed daemon prerequisite can exhaust the socket's trigger limit. A
# later successful feature application must reopen it, even on matching records.
set -eu
systemctl is-enabled --quiet docker.socket
systemctl is-active --quiet docker.socket
