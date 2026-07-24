#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: discobox-build-root-image OUTPUT.qcow2" >&2
    exit 2
fi

output=$1
if [[ $output != /* ]]; then
    output="$(pwd)/$output"
fi
output_dir=$(dirname "$output")
mkdir -p "$output_dir"

repo_root=$(git rev-parse --show-toplevel)
build_dir=$(mktemp -d)
raw_image=$(mktemp --tmpdir="$output_dir" .discobox-root.XXXXXX.raw)
qcow_image=$(mktemp --tmpdir="$output_dir" .discobox-root.XXXXXX.qcow2)
cleanup() {
    rm -rf "$build_dir"
    rm -f "$raw_image" "$qcow_image"
}
trap cleanup EXIT

root_mib=${DISCOBOX_ROOT_IMAGE_MIB:-4096}
rootfs_tar="$build_dir/rootfs.tar"
rootfs_dir="$build_dir/rootfs"
mkdir -p "$rootfs_dir"

docker-buildx build \
    --platform linux/amd64 \
    --file "$repo_root/server/providers/libkrun/image/Dockerfile" \
    --output "type=tar,dest=$rootfs_tar" \
    "$repo_root"

# The tar exporter preserves the Dockerfile filesystem's numeric ownership.
# Extract and populate ext4 in one fakeroot session so mke2fs observes those
# owners even though this builder remains unprivileged.
export repo_root rootfs_tar rootfs_dir raw_image root_mib
# shellcheck disable=SC2016 # Variables expand in the child fakeroot shell.
fakeroot -- bash -c '
    set -euo pipefail
    tar --numeric-owner -xf "$rootfs_tar" -C "$rootfs_dir"

    # BuildKit supplies /etc/resolv.conf as a bind mount during RUN steps.
    # Install the immutable guest resolver file after exporting the rootfs.
    rm -f "$rootfs_dir/etc/resolv.conf"
    install -m 0644 \
        "$repo_root/server/providers/libkrun/image/resolv.conf" \
        "$rootfs_dir/etc/resolv.conf"

    truncate -s "${root_mib}M" "$raw_image"
    mkfs.ext4 -F -q -L discobox-root -d "$rootfs_dir" "$raw_image"
'
qemu-img convert \
    -f raw \
    -O qcow2 \
    -o compat=1.1,lazy_refcounts=on \
    "$raw_image" \
    "$qcow_image"
chmod 0444 "$qcow_image"
mv -f "$qcow_image" "$output"
echo "$output"
