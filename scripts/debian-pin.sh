#!/usr/bin/env bash
# The repository's one Debian pin: every Debian stage it builds is sid, from
# one image digest and one snapshot.debian.org timestamp (see "Debian" in the
# root DESIGN.md). The two values are written into each Dockerfile, because a
# Dockerfile cannot import another, so this script is what keeps them one.
#
#   debian-pin.sh check            fail unless every copy of the pin agrees and
#                                  every Dockerfile builds on it alone
#   debian-pin.sh bump SNAPSHOT    move every copy to SNAPSHOT (YYYYMMDDTHHMMSSZ)
#                                  and to the newest dated sid-slim image
#                                  from a day before it
#
# Called by `go tool task check:debian` and `go tool task debian:bump`.

set -euo pipefail

cd "${DISCOBOX_ROOT:-$(git rev-parse --show-toplevel)}"

image_repo=public.ecr.aws/docker/library/debian
image_re='debian:sid-[0-9]{8}-slim@sha256:[0-9a-f]{64}'
snapshot_re='DEBIAN_SNAPSHOT=[0-9]{8}T[0-9]{6}Z'

# Every tracked file that carries a copy of the pin. ADRs are history and
# never rewritten.
pinned_files() {
  git grep -lIE "$image_re|$snapshot_re" -- ':!docs/adr/**' || true
}

distinct() {
  git grep -hoIE "$1" -- ':!docs/adr/**' | sort -u
}

check() {
  local failed=0 images snapshots

  images=$(distinct "$image_re")
  snapshots=$(distinct "$snapshot_re")
  if [ "$(printf '%s\n' "$images" | grep -c .)" -ne 1 ]; then
    echo "debian pin: expected one sid image digest, found:" >&2
    printf '%s\n' "$images" | sed 's/^/  /' >&2
    failed=1
  fi
  if [ "$(printf '%s\n' "$snapshots" | grep -c .)" -ne 1 ]; then
    echo "debian pin: expected one snapshot, found:" >&2
    printf '%s\n' "$snapshots" | sed 's/^/  /' >&2
    failed=1
  fi
  # The image's date is the day of the snapshot it was built from, at some
  # time that day, and apt never downgrades what the image already holds, so
  # an image not strictly older than the snapshot's day can be newer than its
  # index and leave exact-version dependencies (libc6-dev on libc6) unmet.
  local image_day snapshot_day
  image_day=$(printf '%s\n' "$images" | sed -nE 's/.*sid-([0-9]{8})-slim.*/\1/p' | head -n 1)
  snapshot_day=$(printf '%s\n' "$snapshots" | sed -nE 's/.*=([0-9]{8})T.*/\1/p' | head -n 1)
  if [ -n "$image_day" ] && [ -n "$snapshot_day" ] && [ "$image_day" -ge "$snapshot_day" ]; then
    echo "debian pin: the sid image ($image_day) is not older than the snapshot's day ($snapshot_day)" >&2
    failed=1
  fi

  # A Dockerfile builds on scratch, on an earlier stage, or on the pin through
  # DEBIAN_IMAGE (DISTRO_IMAGE is the sandbox base's stand-in hook, and
  # defaults to it). Its apt reads sid alone: no other suite, and no apt pin to
  # take one package from elsewhere. Docker's own suite is the one exception,
  # set once, as DOCKER_DEBIAN_SUITE.
  local dockerfile
  while IFS= read -r dockerfile; do
    local bad
    bad=$(awk '
      /^[[:space:]]*#/ { next }
      toupper($1) == "FROM" {
        image = ""
        for (i = 2; i <= NF; i++) {
          if ($i ~ /^--/) continue
          image = $i
          break
        }
        for (j = i + 1; j < NF; j++) if (toupper($j) == "AS") stages[$(j + 1)] = 1
        if (image == "scratch" || image == "${DEBIAN_IMAGE}" || image == "${DISTRO_IMAGE}" || (image in stages)) next
        printf "%s:%d: FROM %s: build on ${DEBIAN_IMAGE}, an earlier stage, or scratch\n", FILENAME, FNR, image
      }
      toupper($1) == "ARG" && $2 ~ /^DISTRO_IMAGE=/ && $2 != "DISTRO_IMAGE=${DEBIAN_IMAGE}" {
        printf "%s:%d: DISTRO_IMAGE defaults to something other than ${DEBIAN_IMAGE}\n", FILENAME, FNR
      }
      /(^|[^A-Za-z_])(trixie|bookworm|bullseye|buster|forky|duke|testing|stable|oldstable|experimental|unstable)([^A-Za-z_-]|$)/ \
          && $0 !~ /^ARG DOCKER_DEBIAN_SUITE=trixie$/ && $0 !~ /stable main/ && $0 !~ /Components: stable/ {
        printf "%s:%d: names a Debian suite other than sid: %s\n", FILENAME, FNR, $0
      }
      /debian:[0-9]|golang:|rust:|preferences\.d|Pin-Priority|Pin:|APT::Default-Release|-t [a-z]+-backports/ {
        printf "%s:%d: another Debian, or an apt pin: %s\n", FILENAME, FNR, $0
      }
      toupper($1) == "ARG" && $2 ~ /^DEBIAN_IMAGE=/ { has_image = 1 }
      toupper($1) == "ARG" && $2 ~ /^DEBIAN_SNAPSHOT=/ { has_snapshot = 1 }
      END {
        if (has_image != has_snapshot) printf "%s: pins the sid image or the snapshot, not both\n", FILENAME
      }
    ' "$dockerfile")
    if [ -n "$bad" ]; then
      printf '%s\n' "$bad" >&2
      failed=1
    fi
  done < <(git ls-files '*Dockerfile' '*.Dockerfile')

  if [ "$failed" -ne 0 ]; then
    echo "debian pin: every Debian stage builds on the one pin; see \"Debian\" in DESIGN.md" >&2
    exit 1
  fi
  echo "debian pin: $(printf '%s' "$images" | sed 's/^debian://'), snapshot ${snapshots#DEBIAN_SNAPSHOT=}"
}

bump() {
  local snapshot=${1:-}
  if ! [[ "$snapshot" =~ ^[0-9]{8}T[0-9]{6}Z$ ]]; then
    echo "usage: debian-pin.sh bump YYYYMMDDTHHMMSSZ (a snapshot listed at https://snapshot.debian.org/archive/debian/)" >&2
    exit 2
  fi
  # A timestamp that is not itself a snapshot redirects to an earlier one, so
  # the pin would name a time nothing was taken at. Insist on an exact one.
  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' "https://snapshot.debian.org/archive/debian/$snapshot/")
  if [ "$code" != 200 ]; then
    echo "debian pin: $snapshot is not a snapshot of sid (HTTP $code); pick one from https://snapshot.debian.org/archive/debian/" >&2
    exit 1
  fi

  # The image is the newest dated sid-slim from a day before the snapshot's:
  # Docker publishes one every few weeks, each named for the day it was built
  # from, and the same day may be later in the day than the snapshot.
  local day=${snapshot%%T*} tag="" digest="" i
  for i in $(seq 1 90); do
    local candidate
    candidate=$(date -u -d "$day - $i days" +%Y%m%d)
    if digest=$(docker buildx imagetools inspect --format '{{json .Manifest}}' "$image_repo:sid-$candidate-slim" 2>/dev/null | jq -r .digest); then
      tag="sid-$candidate-slim"
      break
    fi
  done
  if [ -z "$tag" ] || ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "debian pin: no $image_repo:sid-YYYYMMDD-slim within 90 days before $day" >&2
    exit 1
  fi

  local file
  while IFS= read -r file; do
    sed -i.bak -E \
      -e "s/debian:sid-[0-9]{8}-slim@sha256:[0-9a-f]{64}/debian:$tag@$digest/g" \
      -e "s/DEBIAN_SNAPSHOT=[0-9]{8}T[0-9]{6}Z/DEBIAN_SNAPSHOT=$snapshot/g" \
      "$file"
    rm -f "$file.bak"
  done < <(pinned_files)
  check
}

case "${1:-}" in
check) check ;;
bump) bump "${2:-}" ;;
*)
  echo "usage: debian-pin.sh check | bump SNAPSHOT" >&2
  exit 2
  ;;
esac
