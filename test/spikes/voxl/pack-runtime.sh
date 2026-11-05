#!/usr/bin/env bash
# Carry a Nix-built Linux/x86_64 binary into a disposable Debian sandbox.
# Runtime files are local artifacts only; never commit or publish this bundle.
set -euo pipefail
binary=${1:?usage: pack-runtime.sh VOXL_BINARY OUTPUT_DIR}
output=${2:?usage: pack-runtime.sh VOXL_BINARY OUTPUT_DIR}
mkdir -p "$output/runtime"
cp "$binary" "$output/voxl-native"
ldd "$binary" | awk '/=> \// {print $3} /^[[:space:]]*\/.*ld-linux/ {print $1}' > "$output/libraries.txt"
while IFS= read -r library; do cp -L "$library" "$output/runtime/"; done < "$output/libraries.txt"
cat > "$output/voxl" <<'WRAPPER'
#!/usr/bin/env bash
set -euo pipefail
root=$(cd -- "$(dirname -- "$0")" && pwd)
exec "$root/runtime/ld-linux-x86-64.so.2" --library-path "$root/runtime" "$root/voxl-native" "$@"
WRAPPER
chmod +x "$output/voxl"
