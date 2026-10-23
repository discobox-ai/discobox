#!/usr/bin/env bash
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

# mapfile is bash 4. macOS ships bash 3.2 as /bin/bash and always will (it is
# the last GPLv2 release), so the shebang asks PATH for a bash rather than
# naming that one. Say so plainly if PATH hands us an old one too: the bare
# failure is `mapfile: command not found`, which reads like a missing tool
# rather than a missing bash.
if [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
  echo "[dockerfile-test-builds] needs bash 4+ (this is ${BASH_VERSION:-unknown}); install a newer bash or run inside 'nix develop'" >&2
  exit 1
fi

workspace="${DISCOBOX_ROOT:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$workspace"

# The ordinary build targets publish :local images consumed by the development
# watcher. A verification build must not move those tags out from under a live
# server, so the same targets are parameterized onto an isolated test namespace.
export BASE_IMAGE="discobot-dockerfile-test:base"
export POOL_AGENT_IMAGE="discobot-dockerfile-test:pool-agent"
export PLATFORM_BUNDLE="discobot-dockerfile-test:platform"
export HARNESS_BUNDLE="discobot-dockerfile-test:harness"

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
    server/providers/dockerworker/sandbox-image/Dockerfile)
      run_task "$dockerfile" build:sandbox-base-image
      ;;
    pool-agent/Dockerfile)
      run_task "$dockerfile" build:pool-agent-image
      ;;
    sandbox-agent/Dockerfile)
      # Its one artifact is the platform bundle; the stages before it build
      # what it carries.
      run_task "$dockerfile" build:platform-bundle
      ;;
    harness/*/Dockerfile)
      # The harness that changed: a bundle is FROM scratch and builds alone.
      harness_dir="${dockerfile#harness/}"
      harness_dir="${harness_dir%/Dockerfile}"
      harness="$harness_dir"
      [ "$harness_dir" = codex-cli ] && harness=codex
      run_task "$dockerfile" build:harness-bundle HARNESS="$harness"
      ;;
    test/harness-stub/Dockerfile|test/performance/terminal-latency/image/Dockerfile)
      # Bundles like every harness's, each of which refuses to build without
      # its manifest, under a test tag so a running server's is not moved.
      echo "[dockerfile-test-builds] building $dockerfile with context $(dirname "$dockerfile")"
      DOCKER_BUILDKIT=1 docker build --pull=false \
        --build-arg "HARNESS_METADATA=$(jq -c . "$(dirname "$dockerfile")/image.json")" \
        --tag "discobot-dockerfile-test:$(tag_part "$dockerfile")" \
        --file "$dockerfile" "$(dirname "$dockerfile")"
      ;;
    vm-image/Dockerfile)
      # It installs a kernel, generates an initrd, and runs mkfs.ext4 over the
      # whole root filesystem, for two architectures. Building that here costs
      # far more than this is for, and building it natively on both is what
      # .github/workflows/vm-image.yml already does.
      echo "[dockerfile-test-builds] skipping $dockerfile (built by the VM image workflow)"
      ;;
    vm-image/kernel/Dockerfile)
      # This one compiles Linux. Same reasoning, more so.
      echo "[dockerfile-test-builds] skipping $dockerfile (built by the VM image workflow)"
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
