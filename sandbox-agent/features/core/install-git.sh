#!/bin/sh
# A git that takes a bearer credential: the agent hands each source's origin
# token to git that way (sourceconverge.RunHelper), and only git 2.46 and
# later can take one. Debian 13's is new enough. Ubuntu 24.04's is 2.43, so
# on Ubuntu and what derives from it git comes from the Ubuntu Git
# Maintainers' PPA, whose signing key (E1DD270288B4E6030699E45FA1715D88E1DF1F24)
# is this feature's own copy.
set -eu
export DEBIAN_FRONTEND=noninteractive

if ./git-check.sh; then
    exit 0
fi
# shellcheck source=/dev/null # the box's own
. /etc/os-release
# Ubuntu, or a distribution built on it (Pop!_OS is like "ubuntu debian"),
# by the Ubuntu release it is built on.
case " $ID ${ID_LIKE:-} " in
*" ubuntu "*) ubuntu="${UBUNTU_CODENAME:-$VERSION_CODENAME}" ;;
*) ubuntu= ;;
esac
if [ -n "$ubuntu" ]; then
    install -m 0755 -d /etc/apt/keyrings
    install -m 0644 git-core-ppa.asc /etc/apt/keyrings/git-core-ppa.asc
    list=/etc/apt/sources.list.d/git-core-ppa.list
    echo "deb [signed-by=/etc/apt/keyrings/git-core-ppa.asc] https://ppa.launchpadcontent.net/git-core/ppa/ubuntu ${ubuntu} main" > "$list"
    # A release the PPA does not publish for fails the update, and its list
    # left behind would fail every apt-get update on the box after it.
    if ! apt-get update; then
        rm -f "$list" /etc/apt/keyrings/git-core-ppa.asc
        exit 1
    fi
else
    apt-get update
fi
apt-get install -y --no-install-recommends git
./git-check.sh
