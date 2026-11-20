# shellcheck shell=sh # Sourced by each harness's generated launcher, never run.
#
# The shared harness launcher (ADR 26-11-06-596 §3). Every coding harness's
# discobox-harness-run is generated from its declaration as a short list of
# calls into these functions, in the order the lifecycle runs them: the
# environment, source-scoped memory, folder trust, then the capture runtime's
# preparation under the capture lock, and last the CLI itself.
#
# Installed at /usr/local/libexec/discobox/harness-runtime/launch.sh by every
# coding harness's feature. Nothing here reads a declaration: what differs
# between harnesses arrives as arguments the generator wrote.

# The primary source's durable data, when the box has a source.
DISCOBOX_HARNESS_SOURCE_DATA=/.discobox/data-per-source/primary

# discobox_harness_env_default NAME VALUE sets NAME when it is unset or empty.
discobox_harness_env_default() {
	if [ -z "$(printenv "$1" || true)" ]; then
		export "$1=$2"
	fi
}

# discobox_harness_env_alias NAME FROM sets NAME from FROM, unless NAME is
# already set: a value the user set for themselves is left alone.
discobox_harness_env_alias() {
	from=$(printenv "$2" || true)
	if [ -n "$from" ] && [ -z "$(printenv "$1" || true)" ]; then
		export "$1=$from"
	fi
}

# discobox_harness_dir ENV DEFAULT prints the CLI's own state directory:
# ENV's value when set, otherwise DEFAULT under the home.
discobox_harness_dir() {
	dir=
	if [ -n "$1" ]; then
		dir=$(printenv "$1" || true)
	fi
	if [ -z "$dir" ]; then
		dir="$HOME/$2"
	fi
	printf '%s\n' "$dir"
}

# discobox_harness_memories LABEL NAME prepares the directory memories live in:
# the primary source's durable data, keyed by the uid the way the pool cache
# is (ADR 0094). Every sandbox on a source mounts the same tree whatever uid it
# runs as, so sandboxes that agree on a uid share memories, and one that does
# not can neither write the other's files nor take them over. The mount's root
# is chowned to whoever created a sandbox last, so the partition is made
# through sudo rather than depending on who owns its parent.
#
# It sets DISCOBOX_HARNESS_MEMORIES, and returns non-zero when the box has no
# source or the directory cannot be prepared, which leaves memories local.
discobox_harness_memories() {
	DISCOBOX_HARNESS_MEMORIES=
	[ -d "$DISCOBOX_HARNESS_SOURCE_DATA" ] || return 1
	user_data="$DISCOBOX_HARNESS_SOURCE_DATA/users/$(id -u)"
	memories="$user_data/harnesses/$2/memories"
	# Where memories lived before they were keyed by uid. They are carried
	# over only to the uid that owns them, never claimed by another.
	legacy="$DISCOBOX_HARNESS_SOURCE_DATA/harnesses/$2/memories"
	if ! sudo -n install -d -m 0755 "$DISCOBOX_HARNESS_SOURCE_DATA/users" ||
		! sudo -n install -d -o "$(id -u)" -g "$(id -g)" -m 0700 "$user_data"; then
		printf '%s\n' "discobox: could not prepare source-scoped $1 memories; using local storage" >&2
		return 1
	fi
	if [ ! -e "$memories" ] && [ -d "$legacy" ] &&
		[ "$(stat -c %u "$legacy")" = "$(id -u)" ]; then
		mkdir -p "$(dirname "$memories")"
		cp -a "$legacy" "$memories" ||
			printf '%s\n' "discobox: could not carry over $1 memories from $legacy" >&2
	fi
	mkdir -p "$memories"
	DISCOBOX_HARNESS_MEMORIES=$memories
	DISCOBOX_HARNESS_LEGACY_MEMORIES=$legacy
}

# discobox_harness_memory_managed_setting LABEL NAME FILE KEY points the CLI at
# the source-scoped memories with a managed-settings drop-in, FILE, holding
# {KEY: <directory>}. The path names the uid, so a static policy file cannot
# carry it; as policy, a CLI started from any shell afterwards finds the same
# memory, and nothing in the user's own settings outranks it.
discobox_harness_memory_managed_setting() {
	discobox_harness_memories "$1" "$2" || return 0
	if ! sudo -n install -d -m 0755 "$(dirname "$3")" ||
		! printf '{"%s":"%s"}\n' "$4" "$DISCOBOX_HARNESS_MEMORIES" |
		sudo -n tee "$3" >/dev/null; then
		printf '%s\n' "discobox: could not record source-scoped $1 memories; using local storage" >&2
	fi
}

# discobox_harness_memory_bind_mount LABEL NAME HOME_ENV HOME_DEFAULT TARGET
# binds the source-scoped memories onto TARGET in the CLI's state directory,
# for a CLI that has no setting moving only its memories and refuses a
# symlinked memory root. The rest of that directory (auth, configuration,
# sessions) stays the box's own. Any step that cannot be taken leaves memories
# local, and the CLI still starts.
discobox_harness_memory_bind_mount() {
	discobox_harness_memories "$1" "$2" || return 0
	state=$(discobox_harness_dir "$3" "$4")
	local_memories="$state/$5"
	mkdir -p "$state"
	# Replace links made by an older launcher, but never follow or replace a
	# link owned by something else.
	if [ -L "$local_memories" ]; then
		case "$(readlink "$local_memories")" in
		"$DISCOBOX_HARNESS_MEMORIES" | "$DISCOBOX_HARNESS_LEGACY_MEMORIES")
			unlink "$local_memories"
			;;
		*)
			printf '%s\n' "discobox: $1 memories already link elsewhere; leaving them unchanged" >&2
			return 0
			;;
		esac
	fi
	mkdir -p "$local_memories"
	if ! mountpoint -q "$local_memories"; then
		if [ -n "$(find "$local_memories" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
			printf '%s\n' "discobox: $1 memories already exist locally; leaving them unchanged" >&2
		elif ! sudo -n mount --bind "$DISCOBOX_HARNESS_MEMORIES" "$local_memories"; then
			printf '%s\n' "discobox: could not mount source-scoped $1 memories; using local storage" >&2
		fi
	fi
}

# discobox_harness_trust_system_layer LOCK CAPTURE trusts the working directory
# in the CLI's system layer, as root, so a later direct start of the CLI sees
# it too. Launches from different folders serialize on LOCK so neither loses
# the other's entry. A failure stops the launch: trust is required.
discobox_harness_trust_system_layer() {
	sudo -n flock "$1" "$2" trust "$PWD"
}

# discobox_harness_prepare CAPTURE runs the capture runtime's preparation under
# the capture lock the sandbox agent's export, repair and handoff take: local
# trust and onboarding, the import of a delivered native login, and any
# environment derived from the harness's settings, which it prints as
# NAME=VALUE lines and this exports. A failure to prepare required local state
# stops the launch; a failed import is said and the CLI starts without it.
discobox_harness_prepare() {
	mkdir -p "$HOME/.config/discobox"
	prepared=$(flock -x "$HOME/.config/discobox/harness-capture.lock" "$1" prepare)
	while IFS= read -r assignment; do
		if [ -n "$assignment" ]; then
			# shellcheck disable=SC2163 # A NAME=VALUE line, exported as written.
			export "$assignment"
		fi
	done <<EOF
$prepared
EOF
}

# discobox_harness_run [options] -- WORDS... runs the CLI with the
# harness-run convention's words (ADR 0086 §3): `--resume [prompt...]` on a
# relaunch, otherwise the prompt.
#
#   --name NAME              the harness's name, for messages
#   --executable COMMAND     the CLI
#   --args 'A B'             arguments always passed, before the rest
#   --resume 'A B'           what a relaunch passes in place of the prompt
#   --prompt FORM            positional, separator, option or option-joined
#   --prompt-option OPTION   the option an option form passes the prompt with
#   --leading-at space       a prompt starting with @ leads with a space
#   --retry STATUS 'DELAYS' HOME_ENV HOME_DEFAULT SESSIONS
#                            start again after an exit with STATUS that began
#                            no session
#
# The lists are words the generator validated: no quoting, globbing or
# whitespace inside one, so splitting them on spaces is exact.
discobox_harness_run() {
	name='' executable='' fixed='' resume='' form=positional option='' leading_at=''
	retry_status='' retry_delays='' retry_home_env='' retry_home='' retry_sessions=''
	while [ "$#" -gt 0 ]; do
		case $1 in
		--name) name=$2 && shift 2 ;;
		--executable) executable=$2 && shift 2 ;;
		--args) fixed=$2 && shift 2 ;;
		--resume) resume=$2 && shift 2 ;;
		--prompt) form=$2 && shift 2 ;;
		--prompt-option) option=$2 && shift 2 ;;
		--leading-at) leading_at=$2 && shift 2 ;;
		--retry)
			retry_status=$2 retry_delays=$3 retry_home_env=$4 retry_home=$5 retry_sessions=$6
			shift 6
			;;
		--)
			shift
			break
			;;
		*)
			printf '%s\n' "discobox: unknown launcher option $1" >&2
			return 2
			;;
		esac
	done

	# The prompt trails a resume too (ADR 0086 §4), so a user whose first
	# launch failed can still see what the sandbox was asked to do. The
	# resumed session already contains it, so the resume arguments replace it
	# rather than sending it a second time.
	if [ "${1-}" = "--resume" ]; then
		set -f
		# shellcheck disable=SC2086 # Validated words, split on purpose.
		set -- $resume
		set +f
	elif [ "$#" -gt 0 ]; then
		# A prompt is one prompt, however many words the shell split it
		# into: the command is typed, so `discobox run . fix the failing
		# tests` arrives here as four arguments, and a CLI reading one
		# positional would otherwise be asked to "fix".
		prompt=$1
		shift
		for word in "$@"; do
			prompt="$prompt $word"
		done
		if [ "$leading_at" = space ]; then
			case $prompt in
			@*) prompt=" $prompt" ;;
			esac
		fi
		case $form in
		positional) set -- "$prompt" ;;
		separator) set -- -- "$prompt" ;;
		option) set -- "$option" "$prompt" ;;
		option-joined) set -- "$option=$prompt" ;;
		esac
	fi
	set -f
	# shellcheck disable=SC2086 # Validated words, split on purpose.
	set -- $fixed "$@"
	set +f

	if [ -z "$retry_status" ]; then
		exec "$executable" "$@"
	fi

	# A CLI that exits at bootstrap when its first requests cannot connect
	# ends the box's job with the launch, though a box's first request out
	# can meet a transient failure. A start that failed with retry_status
	# before it began a session is made again. Any other status is the
	# session's or the user's: 0 is a clean exit, over 128 a signal. A
	# session began if a file appeared or grew under the sessions directory
	# during the attempt (a resume appends to the one it resumes): the prompt
	# is in it, so starting again would send it twice. Files are compared by
	# name and size, not time, so no clock has to agree with another.
	sessions="$(discobox_harness_dir "$retry_home_env" "$retry_home")/$retry_sessions"
	attempts=1
	for delay in $retry_delays; do attempts=$((attempts + 1)); done
	attempt=1
	for delay in $retry_delays ''; do
		before=$(find "$sessions" -type f -exec wc -c {} + 2>/dev/null | sort)
		status=0
		"$executable" "$@" || status=$?
		if [ "$status" -ne "$retry_status" ] || [ -z "$delay" ] ||
			[ "$(find "$sessions" -type f -exec wc -c {} + 2>/dev/null | sort)" != "$before" ]; then
			exit "$status"
		fi
		attempt=$((attempt + 1))
		printf '%s\n' "discobox: $name exited before its session started (status $retry_status); starting it again in ${delay}s (attempt $attempt of $attempts)" >&2
		sleep "$delay"
	done
}
