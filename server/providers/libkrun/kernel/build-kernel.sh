#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "usage: discobox-build-kernel OUTPUT-vmlinux" >&2
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
kernel_image=$(mktemp --tmpdir="$output_dir" .discobox-kernel.XXXXXX.vmlinux)
cleanup() {
    rm -rf "$build_dir"
    rm -f "$kernel_image"
}
trap cleanup EXIT

docker-buildx build \
    --platform linux/amd64 \
    --file "$repo_root/server/providers/libkrun/kernel/Dockerfile" \
    --output "type=local,dest=$build_dir" \
    "$repo_root"

install -m 0444 "$build_dir/vmlinux" "$kernel_image"
mv -f "$kernel_image" "$output"
echo "$output"
