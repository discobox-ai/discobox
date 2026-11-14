#!/usr/bin/env bash
# Download every module go.work's modules need into the module cache, retrying
# a download the proxy drops, so the CI steps after it can run with
# GOPROXY=off.
#
# CI starts each job with an empty module cache (.github/actions/task), and Go
# does not retry a module zip whose download fails partway. Without this, the
# first step to load packages (golangci-lint, a build, a test, a tidy) does
# every download, and a stream the proxy resets surfaces as `could not import
# ... (typecheck)` errors, a build failure or a stale go.sum, which read like a
# broken change. Here a dropped download is retried with backoff, and one that
# still fails is reported as a download failure naming the module.
#
# Two passes, each retried. First `go mod download` from the root, in
# workspace mode: every module go.work's modules need to build and test, which
# covers lint, deadcode, the windows/amd64 vet, tests, builds and the tools
# `go tool` builds, and lets `ci:check` run them offline. Then, in each
# workspace module, `GOWORK=off go mod download all`: `verify` tidies every
# module on its own (scripts/go-mod-tidy.sh), which resolves versions the
# workspace does not and loads its whole module graph.
#
# GO_MOD_DOWNLOAD_ATTEMPTS sets how many tries (default 4); the wait between
# them starts at GO_MOD_DOWNLOAD_DELAY seconds (default 5) and doubles.
#
# Run by .github/actions/task before every CI job's target, and by
# `go tool task ci:mod-download` (which ci:check runs first).

set -euo pipefail

attempts="${GO_MOD_DOWNLOAD_ATTEMPTS:-4}"
delay="${GO_MOD_DOWNLOAD_DELAY:-5}"

# The checkout this script is in, not DISCOBOX_ROOT: that is fixed when the
# shell is entered, and from another checkout would download for the wrong one.
cd "$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"

# `go mod download` records the checksum of every module it fetches in
# go.work.sum, or with GOWORK=off in the module's go.sum, including many that
# nothing builds from and whose checksums the tree does not keep. Put each file
# back, and each go.mod with them, so running this outside CI leaves the
# checkout as it was and `verify` still sees a go.mod that is not tidy. The module
# directories come from go.work ("." or "./cli"), read without jq, which a
# Windows runner does not have.
modules="$(go work edit -json | grep -o '"DiskPath": *"[^"]*"' | sed -E 's/.*"DiskPath": *"//; s/"$//')"
log="$(mktemp)"
saved="$(mktemp -d)"
sums="go.work.sum"
for m in $modules; do
  sums="$sums $m/go.mod $m/go.sum"
done
n=0
for f in $sums; do
  [ -f "$f" ] && cp "$f" "$saved/$n"
  n=$((n + 1))
done
restore() {
  local n=0 f
  for f in $sums; do
    if [ -f "$saved/$n" ]; then
      cmp -s "$saved/$n" "$f" || cp "$saved/$n" "$f"
    fi
    n=$((n + 1))
  done
  rm -rf "$log" "$saved"
}
trap restore EXIT

# GitHub workflow command escaping: a message escapes %, CR and LF; a property
# (the title) also escapes : and , since they delimit properties.
escape_message() {
  local s="$1"
  s="${s//%/%25}"
  s="${s//$'\r'/%0D}"
  s="${s//$'\n'/%0A}"
  printf '%s' "$s"
}
escape_property() {
  local s
  s="$(escape_message "$1")"
  s="${s//:/%3A}"
  s="${s//,/%2C}"
  printf '%s' "$s"
}

# fail <what>: annotate the failed download in "$log" and exit. Go reports each
# failed module as `go: <module>@<version>: <error>`. Annotate each, so the
# job's summary names the module and the proxy's error rather than a step
# failure to read through.
fail() {
  local line rest module err reported=0
  while IFS= read -r line; do
    case "$line" in
      "go: "*@*": "*)
        rest="${line#go: }"
        module="${rest%%: *}"
        err="${rest#*: }"
        echo "::error title=$(escape_property "Module download failed: $module")::$(escape_message "$err")"
        reported=1
        ;;
    esac
  done <"$log"
  if [ "$reported" -eq 0 ]; then
    echo "::error title=Module download failed::$(escape_message "$(cat "$log")")"
  fi
  echo "$1 failed after $attempts attempts" >&2
  exit 1
}

# download <dir> <what> <command...>: run the command in <dir> until it
# succeeds, up to $attempts times with a doubling wait; fail on the last.
download() {
  local dir="$1" what="$2" attempt=1 wait="$delay"
  shift 2
  while :; do
    if (cd "$dir" && "$@") 2>"$log"; then
      return 0
    fi
    cat "$log" >&2
    if [ "$attempt" -ge "$attempts" ]; then
      fail "$what"
    fi
    echo "$what failed (attempt $attempt of $attempts); retrying in ${wait}s" >&2
    sleep "$wait"
    attempt=$((attempt + 1))
    wait=$((wait * 2))
  done
}

download . "go mod download" go mod download
for m in $modules; do
  download "$m" "GOWORK=off go mod download all in $m" env GOWORK=off go mod download all
done
