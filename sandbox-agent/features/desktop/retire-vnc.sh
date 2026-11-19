#!/bin/sh
# The desktop no longer serves VNC (ADR 26-11-06-851 §2): voxl streams it
# through the viewer on 6900, and nothing else may answer for it. A box this
# feature provisioned before that still has the VNC server's units, enabled,
# and the websockify helper, and a step taken out of provision.yaml takes
# nothing it laid with it. So this stops and disables them and removes every
# file of theirs this feature or the boot flow laid, and a box that never had
# them is left as it is.
#
# Its packages are removed too, each only when removing it removes nothing else:
# a package something else on the box depends on is kept, and said so, since
# removing it would take that with it. Their own dependencies are left, for
# whatever else uses them. This runs only on a box this feature laid VNC on, so
# the packages are the ones it installed.
#
# With `check` (check-retire-vnc.sh) it changes nothing and fails while
# anything of VNC's is still laid, so a box that has them again is cleaned
# again at its next boot.
set -eu

units_dir=${DISCOBOX_SYSTEMD_DIR:-/etc/systemd/system}
bin_dir=${DISCOBOX_BIN_DIR:-/usr/local/bin}
sockets='x11vnc.socket websockify-proxy.socket'
units="$sockets x11vnc@.service websockify-proxy.service websockify@.service"
helper=$bin_dir/discobox-vnc-websockify
packages='novnc python3-websockify x11vnc'

# laid names each file of VNC's still on the box.
laid() {
  for unit in $units; do
    for path in "$units_dir/$unit" "$units_dir/$unit.d" "$units_dir/sockets.target.wants/$unit"; do
      if [ -e "$path" ] || [ -L "$path" ]; then
        echo "$path"
      fi
    done
  done
  if [ -e "$helper" ] || [ -L "$helper" ]; then
    echo "$helper"
  fi
}

if [ "${1-}" = check ]; then
  left=$(laid)
  if [ -n "$left" ]; then
    echo "VNC is still laid on this box:" >&2
    echo "$left" >&2
    exit 1
  fi
  exit 0
fi

left=$(laid)
if [ -z "$left" ]; then
  exit 0
fi

# Stopped before they are removed, the per-connection instances too: a unit
# whose file is gone cannot be stopped by name any more. Failure is expected
# for whatever was never loaded.
# shellcheck disable=SC2086 # one unit per word
systemctl stop $sockets websockify-proxy.service 'x11vnc@*.service' 'websockify@*.service' 2>/dev/null || true
# shellcheck disable=SC2086
systemctl disable $sockets 2>/dev/null || true

# The packages before the files: a removal that fails (apt's lock held by
# something else, say) fails this step while the files are still there, so
# the check fails too and the next boot tries again.
# shellcheck disable=SC2086 # one package per word
installed=$(dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' $packages 2>/dev/null | awk '$1 == "ii" { print $2 }')
for package in $installed; do
  takes=$(apt-get -s remove "$package" 2>/dev/null | awk '$1 == "Remv" { print $2 }' | tr '\n' ' ')
  if [ "$takes" = "$package " ]; then
    DEBIAN_FRONTEND=noninteractive apt-get remove -y -q "$package"
  else
    echo "keeping $package: removing it would also remove ${takes:-something apt would not say}"
  fi
done

for path in $left; do
  echo "removing $path"
  rm -rf "$path"
done
systemctl daemon-reload 2>/dev/null || true
