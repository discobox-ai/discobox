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

  dockerfile="${dockerfile#./}"
  if [ ! -f "$dockerfile" ]; then
    return 0
  fi

  case "$dockerfile" in
    Dockerfile*|worker-agent/Dockerfile)
      context="."
      ;;
    *)
      context=$(dirname "$dockerfile")
      ;;
  esac

  tag_part=$(sanitize_tag_part "$dockerfile")
  tag="discobot-dockerfile-test:${tag_part}"

  echo "[dockerfile-test-builds] building $dockerfile with context $context"
  DOCKER_BUILDKIT=1 docker build --pull=false --tag "$tag" --file "$dockerfile" "$context"
}

for file in $changed_files; do
  case "$file" in
    *Dockerfile*)
      build_dockerfile "$file"
      ;;
  esac
done
