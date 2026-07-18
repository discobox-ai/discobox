#!/bin/bash
#---
# name: Dockerfile test builds
# type: file
# pattern: "{Dockerfile*,**/Dockerfile*}"
#---

set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to test-build changed Dockerfiles" >&2
  exit 1
fi

workspace="${DISCOBOT_WORKSPACE:-$(pwd)}"
cd "$workspace"

changed_files="${DISCOBOT_CHANGED_FILES:-}"
if [ -z "$changed_files" ]; then
  changed_files=$(find . -name 'Dockerfile*' -type f -printf '%P\n')
fi

sanitize_tag_part() {
  local value="$1"
  value="${value#./}"
  value="${value,,}"
  value=$(printf '%s' "$value" | sed -E 's/[^a-z0-9_.-]+/-/g; s/^[.-]+//; s/[.-]+$//')
  if [ -z "$value" ]; then
    value="dockerfile"
  fi
  printf '%s' "$value"
}

build_dockerfile() {
  local dockerfile="$1"
  local context tag_part tag
  local -a build_args

  dockerfile="${dockerfile#./}"
  case "$dockerfile" in
    .claude/worktrees/*)
      return 0
      ;;
  esac
  if [ ! -f "$dockerfile" ]; then
    return 0
  fi

  case "$dockerfile" in
    Dockerfile*|*/pool-agent/Dockerfile|pool-agent/Dockerfile|*/sandbox-agent/Dockerfile|sandbox-agent/Dockerfile)
      context="."
      ;;
    *)
      context=$(dirname "$dockerfile")
      ;;
  esac

  tag_part=$(sanitize_tag_part "$dockerfile")
  tag="discobot-dockerfile-test:${tag_part}"
  build_args=(--pull=false --tag "$tag" --file "$dockerfile")

  case "$dockerfile" in
    harness/*/Dockerfile)
      if ! docker image inspect discobox-sandbox-agent:local >/dev/null 2>&1; then
        echo "[dockerfile-test-builds] building sandbox-agent base for $dockerfile"
        DOCKER_BUILDKIT=1 docker build --pull=false --tag discobox-sandbox-agent:local --file sandbox-agent/Dockerfile .
      fi
      if ! command -v jq >/dev/null 2>&1; then
        echo "jq is required to build harness image metadata" >&2
        exit 1
      fi
      build_args+=(--build-arg "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local")
      build_args+=(--build-arg "HARNESS_METADATA=$(jq -c .harness "$context/image.json")")
      ;;
  esac

  echo "[dockerfile-test-builds] building $dockerfile with context $context"
  DOCKER_BUILDKIT=1 docker build "${build_args[@]}" "$context"
}

for file in $changed_files; do
  case "$file" in
    *Dockerfile*)
      build_dockerfile "$file"
      ;;
  esac
done
