#!/usr/bin/env bash
# Validate the exported VM artifacts.
set -eu
cd /artifacts
ls -l
test -s vmlinux || { echo "missing or empty: vmlinux" >&2; exit 1; }
# libkrun loads this with KRUN_KERNEL_FORMAT_ELF, so an ELF header is
# the difference between a VM that boots and one that dies with EINVAL.
magic=$(dd if=vmlinux bs=4 count=1 status=none | od -An -tx1 | tr -d ' \n')
test "$magic" = "7f454c46" || { echo "vmlinux is not an ELF image" >&2; exit 1; }
# Docker's iptables-nft frontend needs these built in, not modular:
# libkrunfw ships a compact nftables-only kernel and a guest missing
# them starts every container with no network rules and no error.
# CONFIG_PACKET is the DHCP client's socket family: the guest takes its
# address from a lease on every backend (ADR 0101 §2), so a kernel
# without it boots a guest that runs dockerd and never gets an address.
for required in CONFIG_PACKET=y CONFIG_NFT_COMPAT=y CONFIG_BRIDGE=y CONFIG_VETH=y CONFIG_OVERLAY_FS=y; do
  grep -qx "$required" kernel.config || { echo "kernel is missing $required" >&2; exit 1; }
done
echo "kernel verified"
