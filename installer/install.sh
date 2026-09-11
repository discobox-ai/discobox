#!/bin/sh
# Install the discobox command line client.
#
#   curl -sSfL https://discobox.ai | sh
#   curl -sSfL https://discobox.ai | sh -s -- --channel latest
#   curl -sSfL https://discobox.ai | sh -s -- --version v0.7.1
#   curl -sSfL https://edge.discobox.ai | sh
#
# The copy a release uploads is stamped with that release and the SHA-256 of
# every binary it uploaded, and run with no arguments installs exactly that
# release. Asked for any other version or channel, it downloads the installer
# that release uploaded and hands over to it, so the code installing a release
# is always the code that release shipped (ADR 0109).
#
# Everything happens inside main, called on the last line, so a download cut
# short part way through runs nothing at all.
set -eu

# Stamped by internal/cmd/discobox-installers into the copy a release uploads.
# Empty here in the source tree, where this script can only hand over to a
# release's own installer.
release=
checksums=

# Where release assets come from, tried in order as <base>/<tag>/<asset>: the
# mirror first and the release itself last, every one checked against the same
# digest (ADR 0106). Overridable for a test or a private mirror.
sources=${DISCOBOX_INSTALL_SOURCES:-"https://assets.discobox.ai/discobox https://github.com/discobox-ai/discobox/releases/download"}
# Where a channel is looked up.
api=${DISCOBOX_INSTALL_API:-https://api.github.com/repos/discobox-ai/discobox}
releases_page=https://github.com/discobox-ai/discobox/releases

usage() {
	cat <<'USAGE'
Install the discobox command line client.

Usage: install.sh [--channel CHANNEL | --version VERSION] [--dir DIR]

  --channel CHANNEL  stable  the newest release marked stable
                     latest  the newest vX.Y.Z release, stable or not
                     edge    the newest release of any kind, -alpha and -rc too
  --version VERSION  one release, such as v0.7.1
  --dir DIR          where to put the discobox command
                     (default: /usr/local/bin as root, ~/.local/bin otherwise)

With neither --channel nor --version it installs the release this copy of the
script came with: stable from discobox.ai, edge from edge.discobox.ai.

Each option can also be set in the environment as DISCOBOX_CHANNEL,
DISCOBOX_VERSION, or DISCOBOX_INSTALL_DIR. A flag beats the environment, and a
version beats a channel.
USAGE
}

say() {
	printf '%s\n' "$*" >&2
}

die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "this installer needs $1, and it is not on your PATH"
}

main() {
	need curl
	need uname
	need mktemp

	channel=${DISCOBOX_CHANNEL:-}
	version=${DISCOBOX_VERSION:-}
	dir=${DISCOBOX_INSTALL_DIR:-}
	flag_channel=
	flag_version=
	while [ $# -gt 0 ]; do
		case $1 in
			--channel | --version | --dir)
				[ $# -ge 2 ] || die "$1 needs a value"
				option "$1" "$2"
				shift 2
				;;
			--channel=* | --version=* | --dir=*)
				option "${1%%=*}" "${1#*=}"
				shift
				;;
			-h | --help)
				usage
				exit 0
				;;
			*) die "unknown option $1 (try --help)" ;;
		esac
	done
	if [ -n "$flag_version" ]; then
		version=$flag_version
		channel=
	elif [ -n "$flag_channel" ]; then
		channel=$flag_channel
		version=
	elif [ -n "$version" ]; then
		channel=
	fi

	if [ -n "$version" ]; then
		case $version in
			v*) ;;
			*) version=v$version ;;
		esac
		case $version in
			v[0-9]*) ;;
			*) die "$version is not a release version, such as v0.7.1" ;;
		esac
		case $version in
			*[!0-9A-Za-z.+-]*) die "$version is not a release version, such as v0.7.1" ;;
		esac
	fi
	case $channel in
		'' | stable | latest | edge) ;;
		*) die "there is no channel called $channel: choose stable, latest, or edge" ;;
	esac

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	if [ -n "$version" ]; then
		target=$version
	elif [ -n "$channel" ]; then
		target=$(resolve "$channel")
	elif [ -n "$release" ]; then
		target=$release
	else
		target=$(resolve stable)
	fi

	if [ "$target" = "$release" ]; then
		install_release
		return
	fi
	# A release's installer is always asked for its own release, so a second
	# hand-over means that release uploaded the wrong script. Stop rather than
	# follow it anywhere.
	if [ -n "${DISCOBOX_INSTALL_DELEGATED:-}" ]; then
		die "the installer $DISCOBOX_INSTALL_DELEGATED uploaded is stamped for ${release:-no release}"
	fi
	delegate "$target"
}

option() {
	case $1 in
		--channel) flag_channel=$2 ;;
		--version) flag_version=$2 ;;
		--dir) dir=$2 ;;
	esac
}

# resolve prints the release a channel is at, with the same rules the Homebrew
# tap uses for its two formulae, plus edge (ADR 0109 section 3).
resolve() {
	status=$(curl -sSL --retry 2 -o "$tmp/releases.json" -w '%{http_code}' \
		-H 'Accept: application/vnd.github+json' "$api/releases?per_page=100") ||
		die "could not reach $api to look up the $1 channel"
	case $status in
		200) ;;
		403 | 429) die "GitHub is rate limiting this address, so the $1 channel cannot be looked up right now; pin a release with --version instead (see $releases_page)" ;;
		*) die "$api answered $status when asked for the $1 channel" ;;
	esac

	# One "<tag> <prerelease>" line per release, newest first. No jq, so this
	# leans on two facts about the JSON GitHub returns: each release has one
	# tag_name and one prerelease, and neither key appears anywhere else in it.
	# A quote inside a string is escaped, so a release body that mentions
	# either key cannot match these patterns. Whichever of the two comes first,
	# a release is complete once both have been seen. Only CLI tags count.
	tr -d '\r\n' <"$tmp/releases.json" | tr ',' '\n' |
		sed -n \
			-e 's/^[[:space:]{]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/tag \1/p' \
			-e 's/^[[:space:]{]*"prerelease"[[:space:]]*:[[:space:]]*\([a-z]*\).*/prerelease \1/p' |
		awk '
			$1 == "tag" { tag = $2; seen_tag = 1 }
			$1 == "prerelease" { pre = $2; seen_pre = 1 }
			seen_tag && seen_pre {
				if (tag ~ /^v[0-9]/) print tag, pre
				seen_tag = seen_pre = 0
			}' >"$tmp/releases"

	stable=$(awk '$2 == "false" { print $1; exit }' "$tmp/releases")
	case $1 in
		stable) found=$stable ;;
		latest) found=$(awk '$1 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/ { print $1; exit }' "$tmp/releases") ;;
		edge)
			# The newer of the newest stable release and the newest prerelease,
			# which is what edge.discobox.ai works out from the mirror's two
			# aliases. A stable release is always a dot release, so comparing
			# version cores is enough, and stable wins a tie.
			found=$stable
			pre=$(awk '$2 == "true" { print $1; exit }' "$tmp/releases")
			if [ -n "$pre" ] && { [ -z "$stable" ] || newer "$pre" "$stable"; }; then
				found=$pre
			fi
			;;
	esac
	[ -n "$found" ] || die "there is no release on the $1 channel yet"
	say "the $1 channel is at $found"
	printf '%s\n' "$found"
}

# newer succeeds when the first version's MAJOR.MINOR.PATCH is above the
# second's.
newer() {
	awk -v a="$1" -v b="$2" '
		function core(v) { sub(/^v/, "", v); sub(/[-+].*/, "", v); return v }
		BEGIN {
			split(core(a), x, "."); split(core(b), y, ".")
			for (i = 1; i <= 3; i++) if (x[i] + 0 != y[i] + 0) exit !(x[i] + 0 > y[i] + 0)
			exit 1
		}'
}

# fetch downloads <tag>/<asset> to a file from the first source that has it
# and, given a digest, whose bytes match it.
fetch() {
	# shellcheck disable=SC2086 # the list is split on whitespace on purpose
	for base in $sources; do
		url=$base/$1/$2
		if curl -fsL --retry 2 -o "$3" "$url"; then
			[ -n "$4" ] || return 0
			[ "$(sha256 "$3")" = "$4" ] && return 0
			say "$url is not the file $release was released with; trying the next source"
		fi
	done
	return 1
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "this installer needs sha256sum or shasum to check what it downloads"
	fi
}

delegate() {
	say "installing $1 with the installer it was released with"
	fetch "$1" install.sh "$tmp/install.sh" "" ||
		die "$1 has no installer: there is no such release, or it came before install.sh did (see $releases_page/tag/$1)"
	set -- --version "$1"
	if [ -n "$dir" ]; then
		set -- "$@" --dir "$dir"
	fi
	DISCOBOX_INSTALL_DELEGATED=$2 sh "$tmp/install.sh" "$@" </dev/null
}

# platform sets os and arch to the release's names for this machine, refusing
# one no release can run on.
platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case $os in
		Linux) os=linux ;;
		Darwin) os=darwin ;;
		MINGW* | MSYS* | CYGWIN*) die "on Windows, install from PowerShell instead: irm https://discobox.ai/install.ps1 | iex" ;;
		*) die "discobox is not released for $os" ;;
	esac
	case $arch in
		x86_64 | amd64) arch=amd64 ;;
		aarch64 | arm64) arch=arm64 ;;
		*) die "discobox is not released for $os on $arch" ;;
	esac
	if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
		# A shell running under Rosetta reports x86_64 on Apple Silicon.
		if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
			arch=arm64
		else
			die "discobox needs an Apple Silicon Mac; it is not released for Intel Macs"
		fi
	fi
	if [ "$os" = linux ]; then
		# The loader release:binary names, since a release binary is dynamic
		# whatever CGO_ENABLED says and a musl system has none.
		if [ "$arch" = amd64 ]; then loader=/lib64/ld-linux-x86-64.so.2; else loader=/lib/ld-linux-aarch64.so.1; fi
		[ -e "$loader" ] || die "discobox needs glibc, and $loader is missing (a musl system such as Alpine cannot run it)"
	fi
}

install_release() {
	platform
	asset=discobox-$os-$arch
	expected=$(printf '%s\n' "$checksums" | awk -v asset="$asset" '$2 == asset { print $1 }')
	[ -n "$expected" ] || die "$release has no build for $os on $arch"

	say "downloading discobox $release for $os/$arch"
	fetch "$release" "$asset" "$tmp/discobox" "$expected" ||
		die "could not download $asset for $release from any source with the SHA-256 it was released with"
	chmod 755 "$tmp/discobox"

	if [ -z "$dir" ]; then
		if [ "$(id -u)" = 0 ]; then dir=/usr/local/bin; else dir=${HOME:?}/.local/bin; fi
	fi
	mkdir -p "$dir" 2>/dev/null || die "cannot create $dir; choose another directory with --dir"
	# Copied in beside the destination and renamed over it, so the swap is
	# atomic and a discobox that is running keeps the file it started from.
	staged=$dir/.discobox.$$
	cp "$tmp/discobox" "$staged" 2>/dev/null || die "cannot write to $dir; choose another directory with --dir"
	mv -f "$staged" "$dir/discobox"

	out=$("$dir/discobox" --version </dev/null 2>&1) || die "installed $dir/discobox, but it does not run: $out"
	case $out in
		*"$release"*) ;;
		*) die "installed $dir/discobox, but it says it is '$out' rather than $release" ;;
	esac
	say "installed discobox $release to $dir/discobox"

	case ":${PATH:-}:" in
		*":$dir:"*)
			found=$(command -v discobox 2>/dev/null || true)
			if [ -n "$found" ] && [ "$found" != "$dir/discobox" ]; then
				say "$found comes before it on your PATH, so that is the one 'discobox' runs"
			fi
			;;
		*)
			say "$dir is not on your PATH; add it with:"
			say "  export PATH=\"$dir:\$PATH\""
			;;
	esac
}

main "$@"
