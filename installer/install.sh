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
# is always the code that release shipped (ADR 0110).
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

# How this looks. The palette is the TUI's (cli/internal/tui/theme.go) and the
# mark is the TUI's, drawn only where the terminal will show it: a pipe, a log
# file, TERM=dumb, and NO_COLOR all get plain text, and CLICOLOR_FORCE or
# FORCE_COLOR turns it back on. Messages go to stderr, so a `| sh` leaves
# stdout alone.
color_depth=0
c_reset=
c_dim=
c_mark=
c_ok=
c_warn=
c_err=
sym_step=
sym_ok=
sym_warn=
sym_err=

# Which of the two digest tools this machine has, decided by need_sha256 before
# anything is downloaded.
sha_tool=

# The mark, in 24-bit color and in the nearest xterm-256 indices, escaped for
# printf %b. Written by `go generate ./installer` from the TUI's own cell data;
# see internal/cmd/discobox-installer-logo.
# BEGIN generated logo
logo_24bit='     \033[38;2;139;47;214m\0342\0226\0227\0342\0226\0226\033[0m\n     \033[7;38;2;244;92;255m\0342\0226\0215\033[0m\033[38;2;244;92;255m\0342\0226\0213\033[0m\n     \033[7;38;2;244;92;255m\0342\0226\0214\033[0m\033[38;2;244;92;255m\0342\0226\0213\033[0m  \033[38;2;244;92;255m\0342\0226\0201\0342\0226\0201\033[0m\n      \033[7;38;2;244;92;255m\0342\0226\0204\033[0m\033[38;2;244;92;255m\0342\0226\0206\0342\0226\0207\033[0m\033[7;38;2;244;92;255m\0342\0226\0204\0342\0226\0204\0342\0226\0203\0342\0226\0202\033[0m\033[38;2;244;92;255m\0342\0226\0206\0342\0226\0205\0342\0226\0203\0342\0226\0202\0342\0226\0201\033[0m\n       \033[7;38;2;244;92;255m\0342\0226\0216\033[0m\033[38;2;244;92;255m\0342\0226\0214\033[0m \033[38;2;244;92;255m\0342\0226\0227\0342\0226\0204\0342\0226\0226\033[0m  \033[7;38;2;244;92;255m\0342\0226\0206\0342\0226\0205\0342\0226\0204\0342\0226\0203\0342\0226\0202\033[0m\033[38;2;244;92;255m\0342\0226\0206\0342\0226\0204\033[0m\n       \033[7;38;2;244;92;255m\0342\0226\0215\033[0m\033[38;2;244;92;255m\0342\0226\0214\033[0m \033[38;2;244;92;255m\0342\0226\0235\033[0m\033[7;38;2;244;92;255m\0342\0226\0203\033[0m\033[38;2;244;92;255m\0342\0226\0230\033[0m\033[7;38;2;244;92;255m\0342\0226\0230\033[0m\033[38;2;244;92;255m\0342\0226\0207\0342\0226\0206\033[0m \033[38;2;244;92;255m\0342\0226\0205\0342\0226\0205\033[0m  \033[7;38;2;244;92;255m\0342\0226\0216\033[0m\033[38;2;244;92;255m\0342\0226\0215\033[0m\n        \033[7;38;2;244;92;255m\0342\0226\0226\033[0m\033[38;2;244;92;255m\0342\0226\0204\033[0m   \033[7;38;2;244;92;255m\0342\0226\0207\0342\0226\0206\033[0m  \033[7;38;2;244;92;255m\0342\0226\0204\0342\0226\0204\033[0m \033[38;2;244;92;255m\0342\0226\0227\033[0m\033[7;38;2;244;92;255m \033[0m\033[38;2;139;47;214m\0342\0226\0216\033[0m\n       \033[38;2;139;47;214m\0342\0226\0203\033[0m\033[38;2;244;92;255m\0342\0226\0204\033[0m\033[7;38;2;244;92;255m \033[0m\033[38;2;244;92;255m\0342\0226\0207\0342\0226\0204\0342\0226\0203\0342\0226\0202\033[0m\033[38;2;139;47;214m\0342\0226\0201\033[0m  \033[38;2;244;92;255m\0342\0226\0201\0342\0226\0203\0342\0226\0206\033[0m\033[7;38;2;244;92;255m\0342\0226\0203\0342\0226\0206\0342\0226\0226\033[0m\033[38;2;244;92;255m\0342\0226\0226\033[0m\n      \033[7;38;2;244;92;255m\0342\0226\0213     \0342\0226\0201\0342\0226\0202\0342\0226\0203     \0342\0226\0235\033[0m\033[38;2;139;47;214m\0342\0226\0226\033[0m\033[7;38;2;244;92;255m\0342\0226\0203\033[0m\033[38;2;244;92;255m\0342\0226\0230\033[0m\n      \033[7;38;2;244;92;255m\0342\0226\0214     \033[0m\033[38;2;244;92;255m\0342\0226\0226\033[0m  \033[38;2;244;92;255m\0342\0226\0235\033[0m\033[7;38;2;244;92;255m      \033[0m\033[38;2;244;92;255m\0342\0226\0204\033[0m\n      \033[38;2;139;47;214m\0342\0226\0235\033[0m\033[7;38;2;244;92;255m\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\033[0m\033[7;38;2;139;47;214m\0342\0226\0205\033[0m   \033[38;2;139;47;214m\0342\0226\0235\033[0m\033[7;38;2;244;92;255m\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\033[0m\n'
logo_256='     \033[38;5;92m\0342\0226\0227\0342\0226\0226\033[0m\n     \033[7;38;5;207m\0342\0226\0215\033[0m\033[38;5;207m\0342\0226\0213\033[0m\n     \033[7;38;5;207m\0342\0226\0214\033[0m\033[38;5;207m\0342\0226\0213\033[0m  \033[38;5;207m\0342\0226\0201\0342\0226\0201\033[0m\n      \033[7;38;5;207m\0342\0226\0204\033[0m\033[38;5;207m\0342\0226\0206\0342\0226\0207\033[0m\033[7;38;5;207m\0342\0226\0204\0342\0226\0204\0342\0226\0203\0342\0226\0202\033[0m\033[38;5;207m\0342\0226\0206\0342\0226\0205\0342\0226\0203\0342\0226\0202\0342\0226\0201\033[0m\n       \033[7;38;5;207m\0342\0226\0216\033[0m\033[38;5;207m\0342\0226\0214\033[0m \033[38;5;207m\0342\0226\0227\0342\0226\0204\0342\0226\0226\033[0m  \033[7;38;5;207m\0342\0226\0206\0342\0226\0205\0342\0226\0204\0342\0226\0203\0342\0226\0202\033[0m\033[38;5;207m\0342\0226\0206\0342\0226\0204\033[0m\n       \033[7;38;5;207m\0342\0226\0215\033[0m\033[38;5;207m\0342\0226\0214\033[0m \033[38;5;207m\0342\0226\0235\033[0m\033[7;38;5;207m\0342\0226\0203\033[0m\033[38;5;207m\0342\0226\0230\033[0m\033[7;38;5;207m\0342\0226\0230\033[0m\033[38;5;207m\0342\0226\0207\0342\0226\0206\033[0m \033[38;5;207m\0342\0226\0205\0342\0226\0205\033[0m  \033[7;38;5;207m\0342\0226\0216\033[0m\033[38;5;207m\0342\0226\0215\033[0m\n        \033[7;38;5;207m\0342\0226\0226\033[0m\033[38;5;207m\0342\0226\0204\033[0m   \033[7;38;5;207m\0342\0226\0207\0342\0226\0206\033[0m  \033[7;38;5;207m\0342\0226\0204\0342\0226\0204\033[0m \033[38;5;207m\0342\0226\0227\033[0m\033[7;38;5;207m \033[0m\033[38;5;92m\0342\0226\0216\033[0m\n       \033[38;5;92m\0342\0226\0203\033[0m\033[38;5;207m\0342\0226\0204\033[0m\033[7;38;5;207m \033[0m\033[38;5;207m\0342\0226\0207\0342\0226\0204\0342\0226\0203\0342\0226\0202\033[0m\033[38;5;92m\0342\0226\0201\033[0m  \033[38;5;207m\0342\0226\0201\0342\0226\0203\0342\0226\0206\033[0m\033[7;38;5;207m\0342\0226\0203\0342\0226\0206\0342\0226\0226\033[0m\033[38;5;207m\0342\0226\0226\033[0m\n      \033[7;38;5;207m\0342\0226\0213     \0342\0226\0201\0342\0226\0202\0342\0226\0203     \0342\0226\0235\033[0m\033[38;5;92m\0342\0226\0226\033[0m\033[7;38;5;207m\0342\0226\0203\033[0m\033[38;5;207m\0342\0226\0230\033[0m\n      \033[7;38;5;207m\0342\0226\0214     \033[0m\033[38;5;207m\0342\0226\0226\033[0m  \033[38;5;207m\0342\0226\0235\033[0m\033[7;38;5;207m      \033[0m\033[38;5;207m\0342\0226\0204\033[0m\n      \033[38;5;92m\0342\0226\0235\033[0m\033[7;38;5;207m\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\033[0m\033[7;38;5;92m\0342\0226\0205\033[0m   \033[38;5;92m\0342\0226\0235\033[0m\033[7;38;5;207m\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\0342\0226\0205\033[0m\n'
# END generated logo

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
  --stage            download the server too, rather than on first use

With neither --channel nor --version it installs the release this copy of the
script came with: stable from discobox.ai, edge from edge.discobox.ai.

Each option can also be set in the environment as DISCOBOX_CHANNEL,
DISCOBOX_VERSION, DISCOBOX_INSTALL_DIR, or DISCOBOX_INSTALL_STAGE. A flag beats
the environment, and a version beats a channel.
USAGE
}

# setup_style decides what this terminal can show, once.
setup_style() {
	if [ -n "${NO_COLOR:-}" ]; then return 0; fi
	if [ -z "${CLICOLOR_FORCE:-${FORCE_COLOR:-}}" ]; then
		[ -t 2 ] || return 0
		[ "${TERM:-dumb}" != dumb ] || return 0
	fi

	color_depth=16
	case ${COLORTERM:-} in
		truecolor | 24bit) color_depth=16777216 ;;
	esac
	if [ "$color_depth" -eq 16 ]; then
		case ${TERM:-} in
			*256color* | *direct*) color_depth=256 ;;
			*)
				if command -v tput >/dev/null 2>&1; then
					[ "$(tput colors 2>/dev/null || echo 8)" -ge 256 ] && color_depth=256
				fi
				;;
		esac
	fi

	c_reset=$(printf '\033[0m')
	c_dim=$(printf '\033[2m')
	if [ "$color_depth" -ge 16777216 ]; then
		c_mark=$(printf '\033[38;2;244;92;255m')
	elif [ "$color_depth" -ge 256 ]; then
		# The same colors the TUI names: the mark's purple downsampled, and its
		# own indices for the rest.
		c_mark=$(printf '\033[38;5;207m')
	else
		c_mark=$(printf '\033[95m')
	fi
	if [ "$color_depth" -ge 256 ]; then
		c_ok=$(printf '\033[38;5;83m')
		c_warn=$(printf '\033[38;5;214m')
		c_err=$(printf '\033[38;5;196m')
	else
		c_ok=$(printf '\033[92m')
		c_warn=$(printf '\033[93m')
		c_err=$(printf '\033[91m')
	fi

	if unicode_terminal; then
		sym_step=$(printf '\342\206\222')
		sym_ok=$(printf '\342\234\223')
		sym_warn=$(printf '\342\232\240')
		sym_err=$(printf '\342\234\227')
	else
		sym_step='>'
		sym_ok='+'
		sym_warn='!'
		sym_err='x'
	fi
}

# unicode_terminal reports whether the terminal is being told to expect UTF-8.
# The mark is block characters and the symbols are arrows; neither is worth
# printing as question marks.
unicode_terminal() {
	case ${LC_ALL:-${LC_CTYPE:-${LANG:-}}} in
		*UTF-8* | *UTF8* | *utf-8* | *utf8*) return 0 ;;
	esac
	return 1
}

term_cols() {
	if command -v tput >/dev/null 2>&1; then
		cols=$(tput cols 2>/dev/null || true)
		if [ -n "$cols" ]; then
			printf '%s' "$cols"
			return 0
		fi
	fi
	printf '%s' "${COLUMNS:-80}"
}

# print_logo draws the mark, on the terms the TUI draws it on: it is shading
# rather than line art, so a terminal that cannot color it gets none of it
# rather than a monochrome smear. It also needs the room and the encoding.
print_logo() {
	[ "$color_depth" -ge 256 ] || return 0
	unicode_terminal || return 0
	[ "$(term_cols)" -ge 29 ] || return 0
	if [ "$color_depth" -ge 16777216 ]; then
		printf '\n%b\n' "$logo_24bit" >&2
	else
		printf '\n%b\n' "$logo_256" >&2
	fi
}

say() {
	printf '%s\n' "$*" >&2
}

# step, ok, and warn are the three things this has to say while it works. Each
# falls back to the sentence alone, which is what a log file or a pipe gets.
step() {
	if [ -n "$c_dim" ]; then
		printf '%s%s %s%s\n' "$c_dim" "$sym_step" "$*" "$c_reset" >&2
	else
		say "$*"
	fi
}

ok() {
	if [ -n "$c_ok" ]; then
		printf '%s%s%s %s\n' "$c_ok" "$sym_ok" "$c_reset" "$*" >&2
	else
		say "$*"
	fi
}

warn() {
	if [ -n "$c_warn" ]; then
		printf '%s%s%s %s\n' "$c_warn" "$sym_warn" "$c_reset" "$*" >&2
	else
		say "$*"
	fi
}

die() {
	if [ -n "$c_err" ]; then
		printf '%s%s error:%s %s\n' "$c_err" "$sym_err" "$c_reset" "$*" >&2
	else
		printf 'error: %s\n' "$*" >&2
	fi
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "this installer needs $1, and it is not on your PATH"
}

main() {
	setup_style
	need curl
	need uname
	need mktemp
	need_sha256

	channel=${DISCOBOX_CHANNEL:-}
	version=${DISCOBOX_VERSION:-}
	dir=${DISCOBOX_INSTALL_DIR:-}
	stage=${DISCOBOX_INSTALL_STAGE:-}
	flag_channel=
	flag_version=
	while [ $# -gt 0 ]; do
		case $1 in
			--stage)
				stage=yes
				shift
				;;
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
# tap uses for its two formulae, plus edge (ADR 0110 section 3).
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
	# A quote inside a string is escaped, so a release body that mentions either
	# key cannot match these patterns, the leading [^"]* cannot cross the quote
	# that opens the string it would be inside. Whichever key comes first, a
	# release is complete once both have been seen.
	tr -d '\r\n' <"$tmp/releases.json" | tr ',' '\n' |
		sed -n \
			-e 's/^[^"]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/tag \1/p' \
			-e 's/^[^"]*"prerelease"[[:space:]]*:[[:space:]]*\([a-z]*\).*/prerelease \1/p' \
			>"$tmp/keys"

	# Pairing has no way to see a release boundary, so a key these patterns
	# missed would pair the next release's tag with this one's flag and every
	# pairing after it would be off by one, a prerelease recorded as blessed,
	# installed with no error anywhere. The counts are what catch that: one of
	# each per release, or this answer is not one this can read.
	tags=$(grep -c '^tag ' "$tmp/keys" || true)
	flags=$(grep -c '^prerelease ' "$tmp/keys" || true)
	if [ "${tags:-0}" -ne "${flags:-0}" ]; then
		die "could not read the release list from $api: $tags releases state a tag and $flags state whether they are a prerelease. Pin a release with --version instead (see $releases_page)"
	fi

	# Only CLI tags count: the vm/* and vm-kernel/* lines are their own
	# releases and no installer installs them.
	awk '
		$1 == "tag" { tag = $2; seen_tag = 1 }
		$1 == "prerelease" { pre = $2; seen_pre = 1 }
		seen_tag && seen_pre {
			if (tag ~ /^v[0-9]/) print tag, pre
			seen_tag = seen_pre = 0
		}' <"$tmp/keys" >"$tmp/releases"

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
	step "the $1 channel is at $found"
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
			warn "$url is not the file $release was released with; trying the next source"
		fi
	done
	return 1
}

# need_sha256 picks the digest tool up front. Deciding it inside sha256 would
# put the die() inside the command substitution that hashes a download, where it
# kills the subshell and nothing else: every source would then be reported as
# serving bytes that do not match, which is a lie about the mirror and about
# GitHub, for a machine that simply cannot hash anything.
need_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha_tool=sha256sum
	elif command -v shasum >/dev/null 2>&1; then
		sha_tool=shasum
	else
		die "this installer needs sha256sum or shasum to check what it downloads"
	fi
}

sha256() {
	case $sha_tool in
		sha256sum) sha256sum "$1" | cut -d' ' -f1 ;;
		*) shasum -a 256 "$1" | cut -d' ' -f1 ;;
	esac
}

delegate() {
	step "installing $1 with the installer it was released with"
	fetch "$1" install.sh "$tmp/install.sh" "" ||
		die "$1 has no installer: there is no such release, or it came before install.sh did (see $releases_page/tag/$1)"
	set -- --version "$1"
	if [ -n "$dir" ]; then
		set -- "$@" --dir "$dir"
	fi
	if [ -n "$stage" ]; then
		set -- "$@" --stage
	fi
	DISCOBOX_INSTALL_DELEGATED=$2 sh "$tmp/install.sh" "$@" </dev/null
}

# server_note says where the other half comes from, as the Homebrew formula's
# caveats do. The server is not in this download and never was: the CLI fetches
# the one it was cut against, checked against digests it carries (ADR 0099).
server_note() {
	say ""
	say "The Discobox server is a separate program. The CLI downloads the one it"
	say "was cut against the first time something needs a server on this machine,"
	say "keeps it under your state directory by version, and checks it against the"
	say "SHA-256 it carries for it. To do that now rather than then:"
	say ""
	say "  $dir/discobox admin server stage      download and verify it now"
	say "  $dir/discobox admin server manifest   show exactly what that fetches"
}

# stage_server does that download here, for --stage. The install itself has
# already succeeded, so a failure says so rather than reading as a broken
# install, but it is still a failure, because it is what was asked for.
stage_server() {
	step "staging the server discobox $release runs"
	if ! "$dir/discobox" admin server stage </dev/null >&2; then
		die "discobox $release is installed at $dir/discobox, but staging its server failed. Run '$dir/discobox admin server stage' to try again."
	fi
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

	print_logo
	# The name of the thing being installed, under its own mark. No symbol: a
	# tick here would claim something finished before anything has happened.
	if [ -n "$c_mark" ]; then
		printf '%s\033[1mdiscobox %s%s\n\n' "$c_mark" "$release" "$c_reset" >&2
	fi
	step "downloading discobox $release for $os/$arch"
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
	ok "installed discobox $release to $dir/discobox"

	if [ -n "$stage" ]; then
		stage_server
	else
		server_note
	fi

	case ":${PATH:-}:" in
		*":$dir:"*)
			found=$(command -v discobox 2>/dev/null || true)
			if [ -n "$found" ] && [ "$found" != "$dir/discobox" ]; then
				warn "$found comes before it on your PATH, so that is the one 'discobox' runs"
			fi
			;;
		*)
			warn "$dir is not on your PATH; add it with:"
			say "  export PATH=\"$dir:\$PATH\""
			;;
	esac
}

main "$@"
