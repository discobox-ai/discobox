#!/usr/bin/env bash
# Validate the exported VM artifacts.
set -eu
arch="${1:?platform required}"
arch="${arch#linux/}"
cd /artifacts
ls -l
for artifact in initrd.img root.ext4; do
  test -s "$artifact" || { echo "missing or empty: $artifact" >&2; exit 1; }
done
# Only arm64 publishes a kernel. amd64 is booted by libkrun, which needs
# the patched kernel from vm-image/kernel and would not load Debian's;
# see assemble-guest-image.
if [ "$arch" = arm64 ]; then
  test -s vmlinux || { echo "missing or empty: vmlinux" >&2; exit 1; }
  # A hypervisor that loads a kernel directly requires an uncompressed
  # arm64 Image, which carries "ARMd" at offset 0x38.
  magic=$(dd if=vmlinux bs=1 skip=56 count=4 status=none)
  test "$magic" = "ARMd" || { echo "vmlinux is not an uncompressed arm64 kernel Image" >&2; exit 1; }
else
  test ! -e vmlinux || { echo "amd64 published a vmlinux; libkrun boots its own kernel" >&2; exit 1; }
fi
dumpe2fs -h root.ext4 | grep -q "Filesystem volume name:   discobox-root"
# The guest's own name. The Debian base image ships /etc/hostname
# saying "debuerreotype" and a container never sees it, so this is only
# ever wrong in the artifact that boots — the one place nothing looks.
hostname=$(debugfs -R "cat /etc/hostname" root.ext4 2>/dev/null | tr -d '\0\r\n ')
test "$hostname" = "discobox" || { echo "root.ext4 boots as '$hostname', not discobox" >&2; exit 1; }
# One guest for every backend, so the network configuration has to be
# the one both can use: passt and Virtualization.framework's NAT both
# serve DHCP, and a guest that configured an address by hand would boot
# on exactly one of them.
network=$(debugfs -R "cat /etc/systemd/network/10-discobox.network" root.ext4 2>/dev/null)
echo "$network" | grep -q "DHCP=ipv4" \
  || { echo "root.ext4 does not take its address from DHCP" >&2; exit 1; }
# And it has to match the interface both backends actually present:
# virtio-pci is enp0s*, libkrun's virtio-mmio NIC stays eth0.
echo "$network" | grep -q "Name=.*en\*.*eth\*" \
  || { echo "root.ext4 matches only some of the interface names a backend presents" >&2; exit 1; }
# systemd-networkd-wait-online is what gives network-online.target a
# meaning under networkd, and docker.service orders itself after that
# target. Masked, the target is satisfied vacuously and dockerd races
# the lease this guest takes its address, route and resolver from. That
# failure is worse than getting the address wrong: not dead on one
# backend, but intermittent on both.
if debugfs -R "stat /etc/systemd/system/systemd-networkd-wait-online.service" root.ext4 2>/dev/null | grep -q "/dev/null"; then
  echo "systemd-networkd-wait-online.service is masked; dockerd will race the DHCP lease" >&2
  exit 1
fi
if ! debugfs -R "ls /etc/systemd/system/network-online.target.wants" root.ext4 2>/dev/null | grep -q "systemd-networkd-wait-online.service"; then
  echo "systemd-networkd-wait-online.service is not enabled" >&2
  exit 1
fi
echo "VM artifacts verified for $arch (hostname $hostname)"
