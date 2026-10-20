#!/bin/sh
set -eu
export DEBIAN_FRONTEND=noninteractive

extrepo enable mise
apt-get update
apt-get install -y --no-install-recommends mise
