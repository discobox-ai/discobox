#!/bin/bash
# Test-build the repository's Dockerfiles, to catch the breakage that comes from
# changes made nowhere near them: a moved directory leaves `COPY x ./x` naming
# nothing, and a renamed build argument leaves a FROM resolving to nothing.
#
# Called by `go tool task check:dockerfile-builds` and by the Discobot Dockerfile
# hook. With no arguments every tracked Dockerfile is built; arguments are
# changed file paths, and anything that is not a Dockerfile is ignored.
#
# Almost every image here already has a build target that knows its context, its
# build arguments, and what it has to be built after. Those targets are what this
# runs, so that knowledge lives in the Taskfile and nowhere else — an image whose
# build changes must not also have to be changed here. Only the images with no
# target of their own are built directly, at the bottom.

set -euo pipefail

workspace="${DISCOBOX_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$workspace"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to test-build Dockerfiles" >&2
  exit 1
fi

files=("$@")
if [ "${#files[@]}" -eq 0 ]; then
  # Only Dockerfiles this repository owns. A plain find also picks up ignored
  # build output — build/bats-core is a full upstream clone, and its
  # .devcontainer/Dockerfile is not ours to build or keep building.
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    mapfile -t files < <(git ls-files -- 'Dockerfile*' '**/Dockerfile*')
  else
    mapfile -t files < <(find . -name 'Dockerfile*' -type f -printf '%P\n')
  fi
fi

# Each target is run once however many of the Dockerfiles under it changed.
declare -A ran
run_task() {
  local dockerfile="$1"; shift
  local key="$*"
  if [ -n "${ran[$key]:-}" ]; then
    echo "[dockerfile-test-builds] $dockerfile -> already covered by: go tool task $key"
    return 0
  fi
  ran[$key]=1
  echo "[dockerfile-test-builds] $dockerfile -> go tool task $key"
  go tool task "$@"
}

build_dockerfile() {
  local dockerfile="${1#./}"

  [ -f "$dockerfile" ] || return 0

  case "$dockerfile" in
    .claude/worktrees/*)
      return 0
      ;;
    base-image/Dockerfile)
      run_task "$dockerfile" build:base-image
      ;;
    pool-agent/Dockerfile)
      run_task "$dockerfile" build:pool-agent-image
      ;;
    sandbox-agent/Dockerfile)
      run_task "$dockerfile" build:sandbox-agent-image
      ;;
    harness/*/Dockerfile)
      # Every harness at once, rather than build:harness-image for the one that
      # changed: that target takes a sandbox agent image as an argument and does
      # not build one, so a harness alone fails on a daemon that has never built
      # the agent. build:harness-images is where that ordering already lives.
      # The harnesses that did not change are cache hits.
      run_task "$dockerfile" build:harness-images
      ;;
    test/harness-stub/Dockerfile)
      run_task "$dockerfile" build:harness-stub-image
      ;;
    test/performance/terminal-latency/image/Dockerfile)
      run_task "$dockerfile" build:terminal-latency-image
      ;;
    server/providers/vz/image/Dockerfile)
      # arm64-only (ADR 0052 defers Intel Macs), and it installs a kernel,
      # generates an initrd, and runs mkfs.ext4 over the whole root filesystem.
      # Building that under emulation costs far more than this is for, and
      # building it natively is what .github/workflows/vm-image.yml already does
      # on an arm64 runner.
      echo "[dockerfile-test-builds] skipping $dockerfile (arm64-only; built by the VM image workflow)"
      ;;
    server/providers/libkrun/*/Dockerfile)
      # No build target: these produce artifacts the repository deliberately
      # does not build for anyone (see test:e2e:libkrun, which fails listing
      # what is missing rather than building it). Both read from the repository
      # root, unlike everything else with no target.
      echo "[dockerfile-test-builds] building $dockerfile with context ."
      DOCKER_BUILDKIT=1 docker build --pull=false \
        --tag "discobot-dockerfile-test:$(tag_part "$dockerfile")" \
        --file "$dockerfile" .
      ;;
    *)
      # Test fixtures and anything new: a self-contained image whose context is
      # the directory it sits in. An image that needs more than that should get
      # a build target rather than a branch here.
      echo "[dockerfile-test-builds] building $dockerfile with context $(dirname "$dockerfile")"
      DOCKER_BUILDKIT=1 docker build --pull=false \
        --tag "discobot-dockerfile-test:$(tag_part "$dockerfile")" \
        --file "$dockerfile" "$(dirname "$dockerfile")"
      ;;
  esac
}

# A Docker tag takes a narrower alphabet than a path does.
tag_part() {
  local value="${1#./}"
  value="${value,,}"
  value=$(printf '%s' "$value" | sed -E 's/[^a-z0-9_.-]+/-/g; s/^[.-]+//; s/[.-]+$//')
  printf '%s' "${value:-dockerfile}"
}

for file in "${files[@]}"; do
  case "$file" in
    *Dockerfile*) build_dockerfile "$file" ;;
  esac
done
