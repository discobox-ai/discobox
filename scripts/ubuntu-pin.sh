#!/usr/bin/env bash
# The repository's one Ubuntu pin: every Linux stage it builds is Ubuntu 26.04
# LTS (resolute), from one image digest, with apt reading the release's live
# pockets from an ordered mirror list per architecture (see "Ubuntu" in the
# root DESIGN.md). The values are
# written into each Dockerfile, because a Dockerfile cannot import another, so
# this script is what keeps them one:
#
#   UBUNTU_IMAGE          a dated ubuntu:resolute-YYYYMMDD[.N] by digest
#   UBUNTU_MIRROR_AMD64   the mirror apt reads first on amd64
#   UBUNTU_MIRROR_ARM64   the mirror apt reads first on arm64 (ubuntu-ports)
#
# Two pins sit beside it, each moving on its own clock:
#
#   Docker CE             DOCKER_CE_VERSION, CONTAINERD_IO_VERSION and the key
#                         Docker signs them with, DOCKER_APT_KEY_FINGERPRINT,
#                         wherever Docker is installed (the sandbox base and
#                         the pool VM guest)
#   Go                    GO_VERSION, GO_SHA256_AMD64 and GO_SHA256_ARM64,
#                         upstream's release, wherever Go is installed (the Go
#                         builders and the sandbox base)
#
#   ubuntu-pin.sh check               fail unless every copy of every pin
#                                     agrees and every Dockerfile builds on
#                                     them alone
#   ubuntu-pin.sh bump [TAG]          move every copy of the image pin to
#                                     ubuntu:TAG (resolute-YYYYMMDD[.N]), by
#                                     default the newest dated resolute image
#   ubuntu-pin.sh docker-bump [CE [CONTAINERD]]
#                                     move every copy of Docker's pin to
#                                     docker-ce CE and containerd.io CONTAINERD
#                                     (upstream versions, such as 29.9.3-1 and
#                                     2.4.2-4), each defaulting to the newest
#                                     Docker's suite publishes for both amd64
#                                     and arm64
#   ubuntu-pin.sh go-bump [VERSION]   move every copy of Go's pin to VERSION
#                                     (such as 1.27.3), by default the newest
#                                     stable Go, with its published checksums
#
# Called by `go tool task check:ubuntu`, `go tool task ubuntu:bump`,
# `go tool task docker:bump` and `go tool task go:bump`.

set -euo pipefail

cd "${DISCOBOX_ROOT:-$(git rev-parse --show-toplevel)}"

release=resolute
image_repo=public.ecr.aws/docker/library/ubuntu
image_re="ubuntu:$release-[0-9]{8}(\\.[0-9]+)?@sha256:[0-9a-f]{64}"
mirror_amd64_re='UBUNTU_MIRROR_AMD64=[a-z]+://[^[:space:]]+'
mirror_arm64_re='UBUNTU_MIRROR_ARM64=[a-z]+://[^[:space:]]+'
docker_ce_re='DOCKER_CE_VERSION=[0-9][0-9A-Za-z.+-]*'
containerd_io_re='CONTAINERD_IO_VERSION=[0-9][0-9A-Za-z.+-]*'
docker_key_re='DOCKER_APT_KEY_FINGERPRINT=[0-9A-F]{40}'
go_version_re='GO_VERSION=[0-9][0-9A-Za-z.]*'
go_amd64_re='GO_SHA256_AMD64=[0-9a-f]{64}'
go_arm64_re='GO_SHA256_ARM64=[0-9a-f]{64}'
# The lines that look up Docker's pinned versions and check Docker's key.
docker_lookup_re='apt-cache madison (docker-ce|containerd\.io)|gpg --show-keys .*(/etc/apt/keyrings/docker\.asc|/docker\.asc)'

# files RE lists every tracked file outside the ADRs that carries RE. ADRs are
# history and never rewritten.
files() {
  git grep -lIE "$1" -- ':!docs/adr/**' || true
}

distinct() {
  git grep -hoIE "$1" -- ':!docs/adr/**' | sort -u
}

dockerfiles() {
  git ls-files '*Dockerfile' '*.Dockerfile'
}

# one RE WHAT fails unless exactly one value of RE is written anywhere.
one() {
  local values
  values=$(distinct "$1")
  if [ "$(printf '%s\n' "$values" | grep -c .)" -ne 1 ]; then
    echo "ubuntu pin: expected one $2, found:" >&2
    printf '%s\n' "$values" | sed 's/^/  /' >&2
    return 1
  fi
}

# same_copies NAME START END fails unless every copy of a block written into
# the Dockerfiles, from a line matching START to the next matching END, is the
# same text once indentation and the `&&` or `;` joining its commands are set
# aside. A fix to one copy has to reach them all.
same_copies() {
  local name=$1 start=$2 end=$3 copies
  # The awk program is awk's, not the shell's: nothing in it is meant to expand.
  # shellcheck disable=SC2016
  copies=$(dockerfiles | xargs awk -v start="$start" -v end="$end" '
    $0 ~ start { copy = 1; body = "" }
    copy {
      line = $0
      sub(/^[[:space:]]*(&& )?/, "", line)
      sub(/[[:space:]]*\\$/, "", line)
      sub(/;$/, "", line)
      body = body line "\n"
    }
    copy && $0 ~ end { copy = 0; printf "%s:%d\t", FILENAME, FNR; system(""); print body | "sha256sum"; close("sha256sum") }
  ')
  if [ "$(printf '%s\n' "$copies" | awk -F'\t' 'NF { print $2 }' | sort -u | grep -c .)" -gt 1 ]; then
    echo "ubuntu pin: $name differs between its copies:" >&2
    printf '%s\n' "$copies" | sed 's/^/  /' >&2
    return 1
  fi
}

check() {
  local failed=0 re

  one "$image_re" "resolute image digest" || failed=1
  one "$mirror_amd64_re" UBUNTU_MIRROR_AMD64 || failed=1
  one "$mirror_arm64_re" UBUNTU_MIRROR_ARM64 || failed=1
  # Docker CE is one version wherever it is installed, so the pool VM guest's
  # daemon is the one its sandboxes run; Go is one version wherever it builds.
  for re in "$docker_ce_re" "$containerd_io_re" "$docker_key_re" "$go_version_re" "$go_amd64_re" "$go_arm64_re"; do
    one "$re" "${re%%=*}" || failed=1
  done

  # The image's own sources have no CA certificates to check TLS with before
  # apt installs them; apt verifies what it reads by the archive's signature.
  local mirror
  for mirror in $(distinct "$mirror_amd64_re|$mirror_arm64_re" | cut -d= -f2); do
    case "$mirror" in
    http://*) ;;
    *)
      echo "ubuntu pin: $mirror is not a plain http mirror; the bare image cannot check TLS" >&2
      failed=1
      ;;
    esac
  done

  # A Dockerfile builds on scratch, on an earlier stage, or on the pin through
  # UBUNTU_IMAGE, and every stage on the pin writes the mirror list before its
  # first apt-get (a stage built on an earlier one inherits the list). Its apt
  # reads resolute alone: no other release, no snapshot, no apt pin or PPA, no
  # index allowed past its expiry, and no source but the mirror list and the
  # three third-party repositories, Docker's, GitHub CLI's and NodeSource's.
  # NodeSource's is build-time only: removed in the layer that writes it, so a
  # box's apt never reads it (ADR 26-11-19-780).
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
        stage = ""
        for (j = i + 1; j < NF; j++) if (toupper($j) == "AS") stage = $(j + 1)
        if (image == "${UBUNTU_IMAGE}") listed = 0
        else if (image in stages) listed = stages[image]
        else if (image == "scratch") listed = 1
        else {
          printf "%s:%d: FROM %s: build on ${UBUNTU_IMAGE}, an earlier stage, or scratch\n", FILENAME, FNR, image
          listed = 1
        }
        if (stage != "") { stages[stage] = listed; current = stage } else current = ""
      }
      # The mirror list, and the Ubuntu sources, are written by the one setup
      # block every copy shares (compared below), and nowhere else; the
      # moving pockets age out through Valid-Until-Max inside it.
      /case "[$][(]dpkg --print-architecture[)]" in/ { block = 1; aged = 0 }
      block && /Valid-Until-Max: [0-9]+/ { aged = 1 }
      block && /echo "apt reads / {
        block = 0
        if (!aged) printf "%s:%d: the mirror list setup gives the -updates and -security pockets no Valid-Until-Max\n", FILENAME, FNR
        listed = 1
        if (current != "") stages[current] = 1
        next
      }
      /^(RUN|[[:space:]]+&&) cat \/etc\/apt\/mirrors\/ubuntu\.list \\$/ { next }
      !block && /\/etc\/apt\/mirrors\/|\/etc\/apt\/sources\.list\.d\/ubuntu\.sources/ {
        printf "%s:%d: the mirror list or Ubuntu'"'"'s sources written outside the mirror list setup: %s\n", FILENAME, FNR, $0
      }
      !block && />+ *\/etc\/apt\/sources\.list/ && !/\/etc\/apt\/sources\.list\.d\/(docker\.sources|github-cli\.list|nodesource\.sources)/ {
        printf "%s:%d: an apt source written outside the mirror list setup: %s\n", FILENAME, FNR, $0
      }
      /[Tt]rusted(: | *=)|allow-insecure|allow-unauthenticated|AllowInsecureRepositories|AllowDowngradeToInsecure|AllowWeakRepositories/ {
        printf "%s:%d: an apt source trusted without its signature: %s\n", FILENAME, FNR, $0
      }
      /apt-get (update|install)/ && !listed {
        printf "%s:%d: apt-get before the stage writes the mirror list\n", FILENAME, FNR
        listed = 1
      }
      /(^|[^A-Za-z_])(sid|trixie|bookworm|forky|noble|jammy|focal|oracular|plucky|questing|stonking)([^A-Za-z_-]|$)/ {
        printf "%s:%d: names a release other than resolute: %s\n", FILENAME, FNR, $0
      }
      /snapshot\.(ubuntu|debian)\.(com|org)|Check-Valid-Until|preferences\.d|Pin-Priority|Pin:|APT::Default-Release|-backports|add-apt-repository|ppa\.launchpad|ppa:/ {
        printf "%s:%d: a snapshot, an index allowed past its age, an apt pin or a PPA: %s\n", FILENAME, FNR, $0
      }
      /(^|[^A-Za-z])(debian|golang|rust):/ {
        printf "%s:%d: another distribution or a language image: %s\n", FILENAME, FNR, $0
      }
      /URIs: / && !/URIs: mirror\+file:\/etc\/apt\/mirrors\/ubuntu\.list/ && !/download\.docker\.com/ && !/deb\.nodesource\.com\/node_/ {
        printf "%s:%d: an apt source that is not the mirror list: %s\n", FILENAME, FNR, $0
      }
      # The NodeSource source is written and removed in one layer: a new
      # instruction while it is still in place leaves it in the image.
      /^[A-Z]+[[:space:]]/ && nodesource_open {
        printf "%s:%d: NodeSource'"'"'s apt source is left in the image: remove it in the layer that writes it\n", FILENAME, nodesource_open
        nodesource_open = 0
      }
      />+ *\/etc\/apt\/sources\.list\.d\/nodesource\.sources/ { nodesource_open = FNR }
      /rm -f[^&]*\/etc\/apt\/sources\.list\.d\/nodesource\.sources/ { nodesource_open = 0 }
      /^[^#]*(^|[^A-Za-z_])deb (\[|http)/ && !/cli\.github\.com\/packages/ {
        printf "%s:%d: an apt source that is not the mirror list: %s\n", FILENAME, FNR, $0
      }
      # Docker from its own repository is the pinned Docker CE, never the
      # newest the suite happens to have.
      /download\.docker\.com/ { docker = 1 }
      toupper($1) == "ARG" && $2 ~ /^DOCKER_CE_VERSION=/ { has_ce = 1 }
      toupper($1) == "ARG" && $2 ~ /^CONTAINERD_IO_VERSION=/ { has_containerd = 1 }
      toupper($1) == "ARG" && $2 ~ /^DOCKER_APT_KEY_FINGERPRINT=/ { has_key = 1 }
      /deb\.nodesource\.com\/node_/ { nodesource = 1 }
      toupper($1) == "ARG" && $2 ~ /^NODE_VERSION=/ { has_node = 1 }
      toupper($1) == "ARG" && $2 ~ /^NODESOURCE_APT_KEY_FINGERPRINT=/ { has_node_key = 1 }
      /go\.dev\/dl\// { go = 1 }
      toupper($1) == "ARG" && $2 ~ /^GO_VERSION=/ { has_go = 1 }
      toupper($1) == "ARG" && $2 ~ /^GO_SHA256_AMD64=/ { has_go_amd64 = 1 }
      toupper($1) == "ARG" && $2 ~ /^GO_SHA256_ARM64=/ { has_go_arm64 = 1 }
      toupper($1) == "ARG" && $2 ~ /^UBUNTU_IMAGE=/ { has_image = 1 }
      toupper($1) == "ARG" && $2 ~ /^UBUNTU_MIRROR_AMD64=/ { has_amd64 = 1 }
      toupper($1) == "ARG" && $2 ~ /^UBUNTU_MIRROR_ARM64=/ { has_arm64 = 1 }
      END {
        if (has_image != has_amd64 || has_image != has_arm64) printf "%s: pins the resolute image or a mirror, not all three\n", FILENAME
        if (docker && !(has_ce && has_containerd && has_key)) printf "%s: installs from Docker'"'"'s repository without ARG DOCKER_CE_VERSION, ARG CONTAINERD_IO_VERSION and ARG DOCKER_APT_KEY_FINGERPRINT\n", FILENAME
        if (nodesource_open) printf "%s:%d: NodeSource'"'"'s apt source is left in the image: remove it in the layer that writes it\n", FILENAME, nodesource_open
        if (nodesource && !(has_node && has_node_key)) printf "%s: installs from NodeSource'"'"'s repository without ARG NODE_VERSION and ARG NODESOURCE_APT_KEY_FINGERPRINT\n", FILENAME
        if (go && !(has_go && has_go_amd64 && has_go_arm64)) printf "%s: installs Go without ARG GO_VERSION, ARG GO_SHA256_AMD64 and ARG GO_SHA256_ARM64\n", FILENAME
      }
    ' "$dockerfile")
    if [ -n "$bad" ]; then
      printf '%s\n' "$bad" >&2
      failed=1
    fi
  done < <(dockerfiles)

  # The mirror list and the Go install are written out wherever they are used.
  same_copies "the apt mirror setup" 'case "[$][(]dpkg --print-architecture[)]" in' 'echo "apt reads ' || failed=1
  same_copies "the Go install" 'go_sha=[$]GO_SHA256_AMD64' '/usr/local/go/bin/go version' || failed=1

  # The lines that turn Docker's pin into the suite's package versions, and the
  # one that checks its key, are copied into each Dockerfile that installs
  # Docker, and they depend on how docker-bump writes the pin: every copy is
  # held to the same text, its indentation and its joining `&&` set aside.
  # Only Docker's key is part of the pin: another key check (the sandbox base's
  # GitHub CLI key) reads a different keyring and is not compared.
  local lookups
  # The sed program matches the literal text `$gnupg` in the Dockerfiles.
  # shellcheck disable=SC2016
  lookups=$(dockerfiles | xargs grep -hE "$docker_lookup_re" |
    sed -E 's/^[[:space:]]*(&& )?//; s/[[:space:]]*\\$//; s/GNUPGHOME=\$gnupg //; s#/etc/apt/keyrings/docker\.asc|/docker\.asc#KEY#' | sort | uniq -c | awk '{ print $1 }' | sort -u)
  if [ -n "$lookups" ] && [ "$lookups" != "$(dockerfiles | xargs grep -l 'download\.docker\.com' | grep -c .)" ]; then
    echo "docker pin: the version lookup or key check differs between the Dockerfiles that install Docker:" >&2
    dockerfiles | xargs grep -nE "$docker_lookup_re" | sed 's/^/  /' >&2
    failed=1
  fi

  if [ "$failed" -ne 0 ]; then
    echo "ubuntu pin: every Linux stage builds on the one pin; see \"Ubuntu\" in DESIGN.md" >&2
    exit 1
  fi
  echo "ubuntu pin: $(distinct "$image_re" | sed 's/^ubuntu://'); mirrors $(distinct "$mirror_amd64_re" | cut -d= -f2) (amd64), $(distinct "$mirror_arm64_re" | cut -d= -f2) (arm64); docker-ce $(distinct "$docker_ce_re" | cut -d= -f2), containerd.io $(distinct "$containerd_io_re" | cut -d= -f2); go $(distinct "$go_version_re" | cut -d= -f2)"
}

# docker_versions PACKAGE prints, oldest first, the upstream versions (epoch
# and the suite's ~ubuntu suffix stripped, so 5:29.9.3-1~ubuntu.26.04~resolute
# is 29.9.3-1) of PACKAGE that Docker's suite publishes for both amd64 and
# arm64: the guest is built for both, so a version on one alone would fail the
# other's build.
docker_versions() {
  local package=$1 arch index
  for arch in amd64 arm64; do
    index=$(curl -fsSL --retry 5 "https://download.docker.com/linux/ubuntu/dists/$release/stable/binary-$arch/Packages") || return 1
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
    echo "docker pin: could not read Docker's $release suite" >&2
    return 1
  }
  if [ -z "$wanted" ]; then
    wanted=$(printf '%s\n' "$versions" | tail -n 1)
  fi
  if [ -z "$wanted" ] || ! printf '%s\n' "$versions" | grep -qxF "$wanted"; then
    echo "docker pin: Docker's $release suite publishes no $package ${wanted:-at all} for both amd64 and arm64" >&2
    return 1
  fi
  if ! [[ "$wanted" =~ ^[0-9][0-9A-Za-z.+-]*$ ]]; then
    echo "docker pin: $package version $wanted is not one this pin can hold" >&2
    return 1
  fi
  printf '%s\n' "$wanted"
}

# rewrite RE REPLACEMENT... applies each sed expression to every file that
# carries a copy of RE.
rewrite() {
  local re=$1 file
  shift
  while IFS= read -r file; do
    sed -i.bak -E "$@" "$file"
    rm -f "$file.bak"
  done < <(files "$re")
}

docker_bump() {
  local ce containerd
  ce=$(docker_version docker-ce "${1:-}") || exit 1
  containerd=$(docker_version containerd.io "${2:-}") || exit 1
  rewrite "$docker_ce_re|$containerd_io_re" \
    -e "s/$docker_ce_re/DOCKER_CE_VERSION=$ce/g" \
    -e "s/$containerd_io_re/CONTAINERD_IO_VERSION=$containerd/g"
  check
}

go_bump() {
  local wanted=${1:-} releases amd64 arm64
  releases=$(curl -fsSL --retry 5 'https://go.dev/dl/?mode=json&include=all') || {
    echo "go pin: could not read Go's release list" >&2
    exit 1
  }
  if [ -z "$wanted" ]; then
    wanted=$(printf '%s\n' "$releases" | jq -r '[.[] | select(.stable)][0].version | ltrimstr("go")')
  fi
  if ! [[ "$wanted" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]; then
    echo "go pin: $wanted is not a stable Go release" >&2
    exit 1
  fi
  sha() {
    printf '%s\n' "$releases" | jq -r --arg v "go$wanted" --arg a "$1" \
      '.[] | select(.version == $v) | .files[] | select(.os == "linux" and .arch == $a and .kind == "archive") | .sha256'
  }
  amd64=$(sha amd64)
  arm64=$(sha arm64)
  if ! [[ "$amd64" =~ ^[0-9a-f]{64}$ && "$arm64" =~ ^[0-9a-f]{64}$ ]]; then
    echo "go pin: Go publishes no go$wanted archive for both linux/amd64 and linux/arm64" >&2
    exit 1
  fi
  rewrite "$go_version_re|$go_amd64_re|$go_arm64_re" \
    -e "s/$go_version_re/GO_VERSION=$wanted/g" \
    -e "s/$go_amd64_re/GO_SHA256_AMD64=$amd64/g" \
    -e "s/$go_arm64_re/GO_SHA256_ARM64=$arm64/g"
  check
}

bump() {
  local tag=${1:-}
  if [ -z "$tag" ]; then
    # Docker publishes a dated image of the release every few weeks; the
    # newest is the one apt has the least to upgrade on top of.
    tag=$(curl -fsSL --retry 5 "https://registry.hub.docker.com/v2/repositories/library/ubuntu/tags?page_size=100&name=$release-" |
      jq -r '.results[].name' | grep -E "^$release-[0-9]{8}(\\.[0-9]+)?$" | sort -V | tail -n 1)
  fi
  if ! [[ "$tag" =~ ^$release-[0-9]{8}(\.[0-9]+)?$ ]]; then
    echo "usage: ubuntu-pin.sh bump [$release-YYYYMMDD[.N]] (a dated tag of $image_repo)" >&2
    exit 2
  fi
  local digest
  digest=$(docker buildx imagetools inspect --format '{{json .Manifest}}' "$image_repo:$tag" | jq -r .digest)
  if ! [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "ubuntu pin: no $image_repo:$tag" >&2
    exit 1
  fi
  rewrite "$image_re" -e "s/$image_re/ubuntu:$tag@$digest/g"
  check
}

case "${1:-}" in
check) check ;;
bump) bump "${2:-}" ;;
docker-bump) docker_bump "${2:-}" "${3:-}" ;;
go-bump) go_bump "${2:-}" ;;
*)
  echo "usage: ubuntu-pin.sh check | bump [TAG] | docker-bump [CE [CONTAINERD]] | go-bump [VERSION]" >&2
  exit 2
  ;;
esac
