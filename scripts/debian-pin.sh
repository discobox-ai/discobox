#!/usr/bin/env bash
# The repository's one Debian pin: every Debian stage it builds is sid, from
# one image digest and one snapshot.debian.org timestamp (see "Debian" in the
# root DESIGN.md). The two values are written into each Dockerfile, because a
# Dockerfile cannot import another, so this script is what keeps them one.
# Docker CE, which Docker publishes no sid suite of, has a pin of its own
# beside it: DOCKER_CE_VERSION, CONTAINERD_IO_VERSION and the key Docker signs
# them with, DOCKER_APT_KEY_FINGERPRINT, one value each wherever Docker is
# installed (the sandbox base and the pool VM guest). It moves on its own
# clock, so a Debian bump never moves Docker, nor a Docker bump Debian.
#
#   debian-pin.sh check            fail unless every copy of both pins agrees
#                                  and every Dockerfile builds on them alone
#   debian-pin.sh bump SNAPSHOT    move every copy of the Debian pin to SNAPSHOT
#                                  (YYYYMMDDTHHMMSSZ) and to the newest dated
#                                  sid-slim image from a day before it
#   debian-pin.sh docker-bump [CE [CONTAINERD]]
#                                  move every copy of Docker's pin to docker-ce
#                                  CE and containerd.io CONTAINERD (upstream
#                                  versions, such as 29.9.3-1 and 2.4.2-4), each
#                                  defaulting to the newest Docker's suite
#                                  publishes for both amd64 and arm64
#
# Called by `go tool task check:debian`, `go tool task debian:bump` and
# `go tool task docker:bump`.

set -euo pipefail

cd "${DISCOBOX_ROOT:-$(git rev-parse --show-toplevel)}"

image_repo=public.ecr.aws/docker/library/debian
image_re='debian:sid-[0-9]{8}-slim@sha256:[0-9a-f]{64}'
snapshot_re='DEBIAN_SNAPSHOT=[0-9]{8}T[0-9]{6}Z'
docker_ce_re='DOCKER_CE_VERSION=[0-9][0-9A-Za-z.+-]*'
containerd_io_re='CONTAINERD_IO_VERSION=[0-9][0-9A-Za-z.+-]*'
docker_key_re='DOCKER_APT_KEY_FINGERPRINT=[0-9A-F]{40}'
# The lines that look up Docker's pinned versions and check Docker's key.
docker_lookup_re='apt-cache madison (docker-ce|containerd\.io)|gpg --show-keys .*(/etc/apt/keyrings/docker\.asc|/docker\.asc)'
docker_suite=trixie

# Every tracked file that carries a copy of the pin. ADRs are history and
# never rewritten.
pinned_files() {
  git grep -lIE "$image_re|$snapshot_re" -- ':!docs/adr/**' || true
}

# Every tracked file that carries a copy of Docker's pin.
docker_pinned_files() {
  git grep -lIE "$docker_ce_re|$containerd_io_re" -- ':!docs/adr/**' || true
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
  # Docker CE is one version wherever it is installed, so the pool VM guest's
  # daemon is the one its sandboxes run.
  local re name values
  for re in "$docker_ce_re" "$containerd_io_re" "$docker_key_re"; do
    name=${re%%=*}
    values=$(distinct "$re")
    if [ "$(printf '%s\n' "$values" | grep -c .)" -ne 1 ]; then
      echo "docker pin: expected one $name, found:" >&2
      printf '%s\n' "$values" | sed 's/^/  /' >&2
      failed=1
    fi
  done

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
      # The image has no CA certificates before apt installs them, so an https
      # snapshot source fails on every machine that does not inject its own.
      /https:\/\/snapshot\.debian\.org/ {
        printf "%s:%d: read the snapshot over http, which apt verifies by signature: %s\n", FILENAME, FNR, $0
      }
      /debian:[0-9]|golang:|rust:|preferences\.d|Pin-Priority|Pin:|APT::Default-Release|-t [a-z]+-backports/ {
        printf "%s:%d: another Debian, or an apt pin: %s\n", FILENAME, FNR, $0
      }
      # Docker from its own repository is the pinned Docker CE, never the
      # newest the suite happens to have.
      /download\.docker\.com/ { docker = 1 }
      toupper($1) == "ARG" && $2 ~ /^DOCKER_CE_VERSION=/ { has_ce = 1 }
      toupper($1) == "ARG" && $2 ~ /^CONTAINERD_IO_VERSION=/ { has_containerd = 1 }
      toupper($1) == "ARG" && $2 ~ /^DOCKER_APT_KEY_FINGERPRINT=/ { has_key = 1 }
      /URIs: http:\/\/snapshot\.debian\.org/ { sources++ }
      /APT::Update::Post-Invoke-Success \{ "\/usr\/local\/sbin\/discobox-apt-snapshot"; \}/ { hooks++ }
      toupper($1) == "ARG" && $2 ~ /^DEBIAN_IMAGE=/ { has_image = 1 }
      toupper($1) == "ARG" && $2 ~ /^DEBIAN_SNAPSHOT=/ { has_snapshot = 1 }
      END {
        if (has_image != has_snapshot) printf "%s: pins the sid image or the snapshot, not both\n", FILENAME
        if (docker && !(has_ce && has_containerd && has_key)) printf "%s: installs from Docker'"'"'s repository without ARG DOCKER_CE_VERSION, ARG CONTAINERD_IO_VERSION and ARG DOCKER_APT_KEY_FINGERPRINT\n", FILENAME
        # Over http with Check-Valid-Until off, any older signed index would
        # pass for the snapshot; the hook binds what apt accepts to it.
        if (sources != hooks) printf "%s: %d snapshot sources but %d discobox-apt-snapshot hooks; each needs one\n", FILENAME, sources, hooks
      }
    ' "$dockerfile")
    if [ -n "$bad" ]; then
      printf '%s\n' "$bad" >&2
      failed=1
    fi
  done < <(git ls-files '*Dockerfile' '*.Dockerfile')

  # The hook is written out wherever a snapshot source is, since a Dockerfile
  # cannot import another; a fix to one copy has to reach them all. Each copy,
  # from the line that computes the snapshot's time to the line that installs
  # the hook, is compared with the first once indentation and the `&&` or `;`
  # joining its commands are set aside.
  local hooks
  # The awk program matches the literal text `$DEBIAN_SNAPSHOT` in the
  # Dockerfiles, so nothing in it is meant to expand.
  # shellcheck disable=SC2016
  hooks=$(git ls-files '*Dockerfile' '*.Dockerfile' | xargs awk '
    /s=\$\(date -u -d "\$\(echo "\$DEBIAN_SNAPSHOT"/ { copy = 1; body = "" }
    copy {
      line = $0
      sub(/^[[:space:]]*(&& )?/, "", line)
      sub(/[[:space:]]*\\$/, "", line)
      sub(/;$/, "", line)
      body = body line "\n"
    }
    copy && /81-discobox-snapshot/ { copy = 0; printf "%s:%d\t", FILENAME, FNR; system(""); print body | "sha256sum"; close("sha256sum") }
  ')
  if [ "$(printf '%s\n' "$hooks" | awk -F'\t' 'NF { print $2 }' | sort -u | grep -c .)" -gt 1 ]; then
    echo "debian pin: the discobox-apt-snapshot hook differs between its copies:" >&2
    printf '%s\n' "$hooks" | sed 's/^/  /' >&2
    failed=1
  fi

  # The lines that turn Docker's pin into the suite's package versions, and the
  # one that checks its key, are copied into each Dockerfile that installs
  # Docker, and they depend on how docker-bump writes the pin: every copy is
  # held to the same text, its indentation and its joining `&&` set aside.
  # Only Docker's key is part of the pin: another key check (the sandbox base's
  # GitHub CLI key) reads a different keyring and is not compared.
  local lookups
  # The sed program matches the literal text `$gnupg` in the Dockerfiles.
  # shellcheck disable=SC2016
  lookups=$(git ls-files '*Dockerfile' '*.Dockerfile' | xargs grep -hE "$docker_lookup_re" |
    sed -E 's/^[[:space:]]*(&& )?//; s/[[:space:]]*\\$//; s/GNUPGHOME=\$gnupg //; s#/etc/apt/keyrings/docker\.asc|/docker\.asc#KEY#' | sort | uniq -c | awk '{ print $1 }' | sort -u)
  if [ -n "$lookups" ] && [ "$lookups" != "$(git ls-files '*Dockerfile' '*.Dockerfile' | xargs grep -l 'download\.docker\.com' | grep -c .)" ]; then
    echo "docker pin: the version lookup or key check differs between the Dockerfiles that install Docker:" >&2
    git ls-files '*Dockerfile' '*.Dockerfile' | xargs grep -nE "$docker_lookup_re" | sed 's/^/  /' >&2
    failed=1
  fi

  if [ "$failed" -ne 0 ]; then
    echo "debian pin: every Debian stage builds on the one pin; see \"Debian\" in DESIGN.md" >&2
    exit 1
  fi
  echo "debian pin: $(printf '%s' "$images" | sed 's/^debian://'), snapshot ${snapshots#DEBIAN_SNAPSHOT=}; docker-ce $(distinct "$docker_ce_re" | cut -d= -f2), containerd.io $(distinct "$containerd_io_re" | cut -d= -f2)"
}

# docker_versions PACKAGE prints, oldest first, the upstream versions (epoch
# and the suite's ~debian suffix stripped, so 5:29.9.3-1~debian.13~trixie is
# 29.9.3-1) of PACKAGE that Docker's suite publishes for both amd64 and arm64:
# the guest is built for both, so a version on one alone would fail the
# other's build.
docker_versions() {
  local package=$1 arch index
  for arch in amd64 arm64; do
    index=$(curl -fsSL --retry 5 "https://download.docker.com/linux/debian/dists/$docker_suite/stable/binary-$arch/Packages") || return 1
    printf '%s\n' "$index" |
      awk -v p="$package" '/^Package: / { pkg = $2 } /^Version: / && pkg == p { print $2 }' |
      sed -E 's/^[0-9]+://; s/~.*$//' | sort -u
  done | sort | uniq -d | sort -V
}

# docker_version PACKAGE [VERSION] prints VERSION if Docker's suite publishes
# it for both architectures, or the newest it does when VERSION is empty.
docker_version() {
  local package=$1 wanted=${2:-} versions
  versions=$(docker_versions "$package") || {
    echo "docker pin: could not read Docker's $docker_suite suite" >&2
    return 1
  }
  if [ -z "$wanted" ]; then
    wanted=$(printf '%s\n' "$versions" | tail -n 1)
  fi
  if [ -z "$wanted" ] || ! printf '%s\n' "$versions" | grep -qxF "$wanted"; then
    echo "docker pin: Docker's $docker_suite suite publishes no $package ${wanted:-at all} for both amd64 and arm64" >&2
    return 1
  fi
  if ! [[ "$wanted" =~ ^[0-9][0-9A-Za-z.+-]*$ ]]; then
    echo "docker pin: $package version $wanted is not one this pin can hold" >&2
    return 1
  fi
  printf '%s\n' "$wanted"
}

docker_bump() {
  local ce containerd file
  ce=$(docker_version docker-ce "${1:-}") || exit 1
  containerd=$(docker_version containerd.io "${2:-}") || exit 1
  while IFS= read -r file; do
    sed -i.bak -E \
      -e "s/$docker_ce_re/DOCKER_CE_VERSION=$ce/g" \
      -e "s/$containerd_io_re/CONTAINERD_IO_VERSION=$containerd/g" \
      "$file"
    rm -f "$file.bak"
  done < <(docker_pinned_files)
  check
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
docker-bump) docker_bump "${2:-}" "${3:-}" ;;
*)
  echo "usage: debian-pin.sh check | bump SNAPSHOT | docker-bump [CE [CONTAINERD]]" >&2
  exit 2
  ;;
esac
