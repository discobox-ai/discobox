#!/usr/bin/env bash
# Moves discovm.DefaultOCIBase, the base discobox sets on every linked VM
# driver whose base is an OCI image (archil), to the sandbox base image a
# release published, by digest:
# ghcr.io/discobox-ai/discobox-sandbox-base:TAG@sha256:...
#
#   oci-base-pin.sh [TAG]   TAG is a release tag (vX.Y.Z[-pre]), by default
#                           the newest dot release tagged in this checkout
#                           whose image is published (a tag can be cut before,
#                           or without, its images)
#
# Called by `go tool task oci-base:pin`. The release publishes the image
# (release:images); this pins it, in a change of its own.
set -euo pipefail

file=server/providers/discovm/base.go
repo=ghcr.io/discobox-ai/discobox-sandbox-base

# digest_of prints the index digest of $repo:$1, or fails when it is not
# published.
digest_of() {
  docker buildx imagetools inspect --format '{{json .Manifest}}' "$repo:$1" 2>/dev/null | jq -r .digest
}

tag=${1:-}
digest=
if [[ -z "$tag" ]]; then
  for candidate in $(git tag --list 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n 10); do
    if digest=$(digest_of "$candidate"); then
      tag=$candidate
      break
    fi
    echo "oci-base-pin: $repo:$candidate is not published; trying the release before it" >&2
  done
  if [[ -z "$tag" ]]; then
    echo "oci-base-pin: none of the ten newest dot releases has a published $repo; pass VERSION" >&2
    exit 1
  fi
fi
if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || [[ "$tag" == *-main.* ]]; then
  echo "oci-base-pin: TAG must be a release tag (vX.Y.Z), not '$tag'" >&2
  exit 1
fi
if [[ -z "$digest" ]] && ! digest=$(digest_of "$tag"); then
  echo "oci-base-pin: $repo:$tag is not published" >&2
  exit 1
fi
if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo "oci-base-pin: $repo:$tag has no digest ($digest)" >&2
  exit 1
fi

ref_re='ghcr\.io/discobox-ai/discobox-sandbox-base:v[^"@]*@sha256:[0-9a-f]{64}'
if [[ $(grep -cE "^const DefaultOCIBase = \"$ref_re\"$" "$file") != 1 ]]; then
  echo "oci-base-pin: $file has no one DefaultOCIBase to move" >&2
  exit 1
fi
tmp=$(mktemp)
sed -E "s|^const DefaultOCIBase = \"$ref_re\"$|const DefaultOCIBase = \"$repo:$tag@$digest\"|" "$file" >"$tmp"
cat "$tmp" >"$file"
rm -f "$tmp"
echo "DefaultOCIBase = $repo:$tag@$digest"
