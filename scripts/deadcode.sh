#!/usr/bin/env bash
# Fail when a function in the workspace is unreachable from every main and test
# program on linux, darwin, and windows alike.
#
# deadcode (golang.org/x/tools/cmd/deadcode) analyses one GOOS at a time, so a
# function only a darwin or windows file calls looks dead on linux. It runs once
# per OS, and only what is dead on all three is reported.
#
# Every module in go.work is analysed in one run: a function that only another
# module calls (the pool agent starting the proxy's control API, say) is live,
# and a run per module would report it.
#
# Not covered, so this is a floor rather than a proof:
#   - constants, types, variables and struct fields: deadcode reports functions;
#   - methods: a method on a type that reaches an interface counts as live
#     whether or not anything calls it;
#   - generated files, which deadcode skips;
#   - a function in an OS-specific file (_linux.go, _windows.go, a build tag):
#     the passes that do not compile the file cannot see it, so it is never
#     dead on all three.
#
# One false positive is possible: the darwin pass runs with cgo off (it is
# cross-compiled, and cgo needs darwin's own toolchain), so the cgo-only files
# of server/providers/vz/internal/vzvm are not analysed. A function only those
# files call is reported dead; it is not, and deleting it breaks the darwin
# build.
#
# termpane's results are dropped: it is a reusable component for other
# programs, so an option nothing here passes is still its API.
#
# The first argument is the deadcode binary, built for the host: `go tool
# deadcode` would build it for the GOOS under analysis.
#
# Called by `go tool task check:deadcode`.

set -euo pipefail

# mapfile is bash 4, and macOS ships bash 3.2 as /bin/bash (see
# scripts/go-mod-tidy.sh).
if [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
  echo "[deadcode] needs bash 4+ (this is ${BASH_VERSION:-unknown}); install a newer bash or run inside 'nix develop'" >&2
  exit 1
fi

deadcode="${1:?usage: deadcode.sh <deadcode binary>}"
# The binary is named relative to where the script was called from; it runs
# from the workspace root.
deadcode="$(cd "$(dirname "$deadcode")" && pwd)/$(basename "$deadcode")"
# The checkout this script is in, not DISCOBOX_ROOT: that is fixed when the
# shell is entered, and from another checkout would analyse the wrong one.
cd "$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"

# Module directories from go.work, as "." or "server", each as a package
# pattern.
mapfile -t work_dirs < <(go work edit -json | grep -o '"DiskPath": *"[^"]*"' | sed -E 's/.*"DiskPath": *"//; s/"$//; s#^\./##')
patterns=()
for dir in "${work_dirs[@]}"; do
  if [ "$dir" = "." ]; then
    patterns+=("./...")
  else
    patterns+=("./$dir/...")
  fi
done

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

goos_list=(linux darwin windows)
for goos in "${goos_list[@]}"; do
  # Positions are relative to the working directory, the workspace root, so a
  # line names the same function in every pass.
  GOOS="$goos" "$deadcode" -test -f '{{range .Funcs}}{{println .Position .Name}}{{end}}' "${patterns[@]}" >"$scratch/$goos.raw"
  # grep exits 1 when it keeps nothing, which is the clean result.
  { grep -v '^termpane/' "$scratch/$goos.raw" || true; } | sort >"$scratch/$goos"
done

cp "$scratch/${goos_list[0]}" "$scratch/dead"
for goos in "${goos_list[@]:1}"; do
  comm -12 "$scratch/dead" "$scratch/$goos" >"$scratch/next"
  mv "$scratch/next" "$scratch/dead"
done

if [ -s "$scratch/dead" ]; then
  echo "Unreachable from every main and test program on ${goos_list[*]}; delete it, or call it." >&2
  echo "(A function only vzvm's cgo files call is the exception: see this script's header.)" >&2
  cat "$scratch/dead" >&2
  exit 1
fi
