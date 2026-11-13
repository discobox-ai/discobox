#!/usr/bin/env bash
# The one live run of a remote driver, end to end (docs/tasks/947, R2): make a
# provider and a pool on DRIVER, start a discobox on it running a harness,
# attach to it, probe its egress and its machine's public URL, keep the
# evidence, delete everything it made, check that nothing is left, and report.
#
# Usage: scripts/live-run.sh --driver DRIVER [flags] [discobox flags...]
#
#   --driver DRIVER     boxd or archil (required)
#   --egress-narrowed yes|no
#                       whether this box's egress was narrowed to the plan's
#                       hosts before the key came in; the report says which
#                       (required, except on a dry run)
#   --harness SLUG      the harness the discobox runs (default: the project's)
#   --allow HOST        the one host the pool's egress allows, which the proxy
#                       must answer for (default: example.com)
#   --unlisted HOST     a host no level allows, which must not answer
#                       (default: example.net)
#   --pool-timeout DUR  how long the pool may take to come up, and anything
#                       deleted to go (default: 20m)
#   --harness-timeout DUR
#                       how long the harness may take to answer (default: 15m)
#   --attach-for DUR    how long to stay attached to the discobox's terminal
#                       (default: 10s)
#   --evidence DIR      where what the run saw is kept, the report among it
#                       (default: ./live-run-<driver>-<nonce>; a dry run's
#                       is removed when it ends)
#   --dry-run           print every call the run would make, with stand-in
#                       answers, and touch nothing: no server, no provider,
#                       and no key read
#
#   go tool task live-run:build    # first, before egress is narrowed
#   go tool task live-run DRIVER=boxd -- --egress-narrowed yes
#   go tool task live-run DRIVER=boxd DRY_RUN=1
#
# The run is made once, in a one-off box, under the rules of the plan's
# Credentials section: only a revision that landed on main, built first
# (live-run:build); the box's egress narrowed next; the key handed over last,
# as a raw binding in the variable disco-vm reads for the driver
# (BOXD_API_KEY, ARCHIL_API_KEY). No agent runs in the box while it holds the
# key. The server is the box's own, run from the pinned build, which the CLI
# reaches on its local socket, never through the box's proxy: the key goes to
# it in the provider's config, the body of a request a raw binding's routes
# would not name. Inside a discobox the CLI's default server is the gate
# ($DISCOBOX_SERVER, api.discobox.internal), the outer server, which must
# never get the key: the run refuses it before anything is sent, so pass the
# box's own server with --server, or unset DISCOBOX_SERVER for the CLI's
# local one. The server calls the provider with the key sealed in
# that config; the box calls the provider itself only through disco-vm, in the
# leftover sweep, on the binding's own routes. It goes from
# the variable into that config through jq's environment, in a 0600 file the
# CLI reads as @file: never on a command line, where /proc/<pid>/cmdline shows
# it, and never printed.
#
# The project's harness credential is a project secret the owner set on the
# server beforehand, so the discobox sees only its sentinel.
#
# A check passes only on what it observed. One whose probe could not reach
# its target (the public URL from a box whose egress does not name its host,
# say) is CHECK, for a person to read in the evidence, and never PASS.
#
# Whatever happens, the evidence is kept and the cleanup runs, in the plan's
# order: the discobox, then the pool (which withdraws its grant of the key),
# then the provider. disco-vm then lists the pool's scope and the provider's
# (machines, committed machines, caches, and keys issued for the pool);
# anything still there is reported, then removed. The exit status is 0 only
# when every check passed and nothing was left.
#
# $DISCOBOX names the CLI (default: discobox on PATH), $DISCO_VM disco-vm's
# (default: disco-vm on PATH; it must be the version the server links, which
# live-run:build builds). Flags the script does not know (--server,
# --project, ...) go to every discobox call.
set -euo pipefail

driver=
narrowed=
harness=
allow_host=example.com
unlisted_host=example.net
pool_timeout=20m
harness_timeout=15m
attach_for=10s
evidence=
dry=0
discobox=${DISCOBOX:-discobox}
discovm=${DISCO_VM:-disco-vm}
dc_flags=()

usage() { sed -n '2,69p' "$0" | sed 's/^# \{0,1\}//'; }
need() { [ "$#" -ge 2 ] || { echo "$1 needs a value" >&2; exit 2; }; }
while [ "$#" -gt 0 ]; do
	case $1 in
	--driver) need "$@"; driver=$2; shift 2 ;;
	--driver=*) driver=${1#*=}; shift ;;
	--egress-narrowed) need "$@"; narrowed=$2; shift 2 ;;
	--egress-narrowed=*) narrowed=${1#*=}; shift ;;
	--harness) need "$@"; harness=$2; shift 2 ;;
	--harness=*) harness=${1#*=}; shift ;;
	--allow) need "$@"; allow_host=$2; shift 2 ;;
	--allow=*) allow_host=${1#*=}; shift ;;
	--unlisted) need "$@"; unlisted_host=$2; shift 2 ;;
	--unlisted=*) unlisted_host=${1#*=}; shift ;;
	--pool-timeout) need "$@"; pool_timeout=$2; shift 2 ;;
	--pool-timeout=*) pool_timeout=${1#*=}; shift ;;
	--harness-timeout) need "$@"; harness_timeout=$2; shift 2 ;;
	--harness-timeout=*) harness_timeout=${1#*=}; shift ;;
	--attach-for) need "$@"; attach_for=$2; shift 2 ;;
	--attach-for=*) attach_for=${1#*=}; shift ;;
	--evidence) need "$@"; evidence=$2; shift 2 ;;
	--evidence=*) evidence=${1#*=}; shift ;;
	--dry-run) dry=1; shift ;;
	-h|--help) usage; exit 0 ;;
	*) dc_flags+=("$1"); shift ;;
	esac
done

# The variable is disco-vm's own name for the driver's key, so the binding
# serves the provider's config and disco-vm's listing alike.
case $driver in
boxd) key_var=BOXD_API_KEY ;;
archil) key_var=ARCHIL_API_KEY ;;
'') echo "--driver is required: boxd or archil" >&2; exit 2 ;;
*) echo "--driver $driver: the live run takes boxd or archil" >&2; exit 2 ;;
esac
case $narrowed in
yes|no) ;;
'') [ "$dry" = 1 ] || { echo "--egress-narrowed yes|no is required: the report says which" >&2; exit 2; }; narrowed="not stated (dry run)" ;;
*) echo "--egress-narrowed takes yes or no" >&2; exit 2 ;;
esac
case $allow_host in "$unlisted_host") echo "--allow and --unlisted name the same host" >&2; exit 2 ;; esac

# seconds turns 90s, 20m or 1h into seconds.
seconds() {
	case $1 in
	*[!0-9smh]*|'') echo "not a duration: $1" >&2; exit 2 ;;
	*h) echo $((${1%h} * 3600)) ;;
	*m) echo $((${1%m} * 60)) ;;
	*s) echo "${1%s}" ;;
	*) echo "$1" ;;
	esac
}
pool_wait=$(seconds "$pool_timeout")
seconds "$harness_timeout" >/dev/null
attach_wait=$(seconds "$attach_for")
[ "$dry" = 0 ] || attach_wait=0

umask 077
# A dry run prints each call on the terminal's stderr, which fd 3 keeps while a
# call's own stderr is captured.
exec 3>&2
work=$(mktemp -d)
results=$work/results
: >"$results"

nonce=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
name=live-$driver-$nonce
# revision says what ran: the commit, whether it is on main as this checkout
# last fetched it, and whether the tree had changes.
revision() {
	local dir head on dirty
	dir=$(dirname "$0")
	head=$(git -C "$dir" rev-parse HEAD 2>/dev/null) || { echo "unknown (not a checkout)"; return; }
	if ! git -C "$dir" rev-parse -q --verify origin/main >/dev/null; then
		on="main unknown (no origin/main)"
	elif git -C "$dir" merge-base --is-ancestor "$head" origin/main; then
		on="on origin/main"
	else
		on="NOT on origin/main"
	fi
	if [ -n "$(git -C "$dir" status --porcelain 2>/dev/null)" ]; then dirty="with uncommitted changes"; else dirty="clean"; fi
	echo "\`$head\`, $on, $dirty"
}

# What ran, read before the run writes anything, its evidence included.
revision=$(revision)
# A dry run's evidence is stand-ins, kept only while it runs unless --evidence
# names somewhere.
if [ -z "$evidence" ] && [ "$dry" = 1 ]; then evidence=$work/evidence; fi
evidence=${evidence:-./live-run-$driver-$nonce}
mkdir -p "$evidence"
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# --- recording ---------------------------------------------------------

# clean puts text on one line with no control characters. Much of what is
# recorded is the guest's (a probe's answer, its screen, a header its URL
# sent), and none of it may drive the terminal of the box holding the key
# (cli/REVIEW.md: text a discobox wrote is escaped before it is printed).
# Only printable ASCII is kept, byte by byte: C1 controls sent as UTF-8 (the
# two-byte CSI, C2 9B) and bidi overrides are not [:cntrl:] bytes, and a
# terminal in UTF-8 obeys them.
clean() { printf '%s' "$1" | LC_ALL=C tr -c ' -~' ' '; }

# record STATUS STEP DETAIL. A dry run's passes and checks are DRY: nothing
# was observed. A failure stays one: it is the script's own.
record() {
	local status=$1 detail
	case $dry:$status in 1:PASS|1:CHECK) status=DRY ;; esac
	detail=$(clean "$3")
	printf '%s\t%s\t%s\n' "$status" "$2" "$detail" >>"$results"
	printf '%-5s %s: %s\n' "$status" "$2" "$detail" >&2
}
pass() { record PASS "$1" "$2"; }
fail() { record FAIL "$1" "$2"; }
# check is an answer the script cannot judge, or a probe that could not be
# made, which a person reads in the evidence. It does not pass.
check() { record CHECK "$1" "$2"; }
note() { printf '      %s\n' "$(clean "$*")" >&2; }
# field JSON FILTER is FILTER's answer on JSON, or nothing when JSON is not.
field() { printf '%s' "$1" | jq -r "$2" 2>/dev/null || true; }
# words puts a list of lines on one line.
words() { printf '%s' "$1" | tr '\n' ' ' | sed 's/ *$//'; }

# --- calls out ---------------------------------------------------------

# ext STATUS CANNED CMD... runs CMD. A dry run prints it instead, and stands
# in CANNED for its output and STATUS for its exit status, so everything that
# reads an answer runs on a dry run too.
ext() {
	local status=$1 canned=$2
	shift 2
	if [ "$dry" = 1 ]; then
		printf '+ %s\n' "$(printf '%q ' "$@")" >&3
		[ -z "$canned" ] || printf '%s\n' "$canned"
		return "$status"
	fi
	"$@"
}
dc() { local status=$1 canned=$2; shift 2; ext "$status" "$canned" "$discobox" ${dc_flags[@]+"${dc_flags[@]}"} "$@"; }
# dcout VAR STATUS CANNED ARGS... runs dc with its stdout in VAR, the part to
# parse, and its stderr (progress lines, warnings) apart, in $err. It returns
# dc's status.
err=
dcout() {
	local var=$1 status=$2 canned=$3 rc=0 got
	shift 3
	got=$(dc "$status" "$canned" "$@" 2>"$work/stderr") || rc=$?
	err=$(tail -c 2000 "$work/stderr")
	printf -v "$var" '%s' "$got"
	return "$rc"
}
# dvm STATUS CANNED SCOPE ... calls disco-vm in a scope of the driver: the
# pool's (its sandboxes' machines) or the provider's (the pool's machine).
dvm() { local status=$1 canned=$2 scope=$3; shift 3; ext "$status" "$canned" "$discovm" --driver "$driver" --pool "$scope" "$@"; }
# pause SECONDS waits between polls; a dry run does not.
pause() { [ "$dry" = 1 ] || sleep "$1"; }

# scope is disco-vm's scope for a discobox ID, as discobox derives it
# (server/providers/discovm machinePool, pool-agent machineName): lower case,
# with _ as -.
scope() { printf '%s' "$1" | tr '[:upper:]_' '[:lower:]-'; }

# --- the run's state, for the cleanup ----------------------------------

provider_id=
pool_id=
box_id=
out=
screen=
answers=

# --- evidence, cleanup and report --------------------------------------

# keep NAME ARGS... saves what a discobox call prints, output and errors, under
# the evidence directory. The cleanup comes after; this run happens once.
keep() {
	local file=$evidence/$1
	shift
	dc 0 "(dry run)" "$@" >"$file" 2>&1 || echo "(exit $?)" >>"$file"
}
collect() {
	echo "--- evidence ($evidence)" >&2
	if [ -n "$box_id" ]; then
		keep discobox.json -o json admin box get "$box_id"
		keep screen.txt capture --scrollback 5000 "$box_id"
	fi
	if [ -n "$pool_id" ]; then
		keep pool.json -o json admin pool get "$pool_id"
		keep pool.log admin pool logs "$pool_id"
	fi
	if [ -n "$provider_id" ]; then keep provider.json -o json admin provider get "$provider_id"; fi
}

# gone STEP KIND ID polls until the server answers that the resource is not
# there. Any other failure (an unreachable server, a refusal) is not that.
gone() {
	local step=$1 kind=$2 id=$3 waited=0 out=
	while :; do
		if out=$(dc 1 'request failed: 404 Not Found' admin "$kind" get "$id" 2>&1); then
			out="it is still there"
		elif printf '%s' "$out" | grep -q 'request failed: 404'; then
			pass "$step" "$kind $id is gone"
			return 0
		fi
		if [ "$waited" -ge "$pool_wait" ]; then
			fail "$step" "$kind $id is not gone after $pool_timeout: $(printf '%s' "$out" | tail -c 300)"
			return 1
		fi
		pause 10
		waited=$((waited + 10))
	done
}

# issued PREFIX reads disco-vm's key ls --json: the IDs of keys named PREFIX….
issued() { printf '%s' "$1" | jq -r --arg p "$2" '.[]? | select(.name | startswith($p)) | .id' 2>/dev/null || true; }
# names reads disco-vm's ls table: the second column, past the header.
names() { printf '%s\n' "$1" | awk 'NR > 1 && $2 != "" { print $2 }'; }

# leftovers STEP SCOPE PREFIX lists what disco-vm still holds in SCOPE:
# machines, committed machines, caches, and keys issued for the pool (named
# PREFIX…, poolkey.go keyPrefix), then removes it. Anything there is a failure
# of the cleanup above, so it is reported.
leftovers() {
	local step=$1 scope=$2 prefix=$3 out machines='' caches='' keys='' unlisted=''
	if out=$(dvm 0 'ID  NAME  FROM  OS  STATE  CREATED' "$scope" ls -a 2>&1); then
		machines=$(names "$out")
	else
		unlisted="machines ($out)"
	fi
	if out=$(dvm 0 '[]' "$scope" cache ls --json 2>&1); then
		caches=$(field "$out" '.[]?.key // empty')
	else
		unlisted="$unlisted caches ($out)"
	fi
	# A driver that issues no keys says so, and so does boxd to an API key,
	# which may neither list keys nor issue them (disco-vm docs/drivers/boxd.md):
	# with the run's key, nothing was issued to leave.
	if out=$(dvm 0 '[]' "$scope" key ls --json 2>&1); then
		keys=$(issued "$out" "$prefix")
	elif ! printf '%s' "$out" | grep -qiE 'PermissionDenied|permission denied|interactive session|not supported'; then
		unlisted="$unlisted keys ($out)"
	fi
	if [ -n "$unlisted" ]; then
		fail "$step-listed" "disco-vm could not list scope $scope's $unlisted"
	fi
	if [ -z "$machines$caches$keys" ]; then
		[ -n "$unlisted" ] || pass "$step" "nothing left in scope $scope"
		return
	fi
	fail "$step" "left in scope $scope: machines [$(words "$machines")] caches [$(words "$caches")] keys [$(words "$keys")]; removing them"
	local n
	for n in $keys; do dvm 0 '' "$scope" key revoke "$n" >&2 || note "could not revoke key $n"; done
	for n in $machines; do dvm 0 '' "$scope" rm --force "$n" >&2 || note "could not remove $n"; done
	for n in $caches; do dvm 0 '' "$scope" cache rm "$n" >&2 || note "could not remove cache $n"; done
	out=$(dvm 0 'ID  NAME  FROM  OS  STATE  CREATED' "$scope" ls -a 2>&1 || true)
	machines=$(names "$out")
	keys=
	if out=$(dvm 0 '[]' "$scope" key ls --json 2>/dev/null); then keys=$(issued "$out" "$prefix"); fi
	if [ -n "$machines$keys" ]; then
		fail "$step-removed" "still left in scope $scope after removing: machines [$(words "$machines")] keys [$(words "$keys")]; remove them by hand"
	else
		pass "$step-removed" "removed what was left in scope $scope"
	fi
}

cleanup() {
	set +e
	collect
	echo "--- cleanup" >&2
	if [ -n "$box_id" ]; then
		if dc 0 '' admin box purge "$box_id" >&2; then
			pass cleanup-discobox "purged $box_id"
		else
			fail cleanup-discobox "could not purge $box_id"
		fi
	fi
	# The pool first: deleting it withdraws its grant of the key (ADR
	# 26-11-11-080 §9), and removes its machine and its sandboxes'.
	if [ -n "$pool_id" ]; then
		if dc 0 '' admin pool rm "$pool_id" >&2; then
			gone cleanup-pool pool "$pool_id"
		else
			fail cleanup-pool "could not delete pool $pool_id"
		fi
	fi
	if [ -n "$provider_id" ]; then
		if dc 0 '' admin provider rm "$provider_id" >&2; then
			gone cleanup-provider provider "$provider_id"
		else
			fail cleanup-provider "could not delete provider $provider_id"
		fi
	fi
	# What the provider holds, read from the provider itself rather than from
	# the server that has just said it is gone. The pool's keys are issued in
	# the provider's scope, named for the pool's.
	if [ -n "$pool_id" ]; then leftovers left-pool-scope "$(scope "$pool_id")" "$(scope "$pool_id")-"; fi
	if [ -n "$provider_id" ]; then leftovers left-provider-scope "$(scope "$provider_id")" "${pool_id:+$(scope "$pool_id")-}"; fi
}

finish() {
	local code=$? failed
	trap - EXIT INT TERM
	cleanup
	failed=$(awk -F'\t' '$1 == "FAIL" || $1 == "CHECK"' "$results" | wc -l | tr -d ' ')
	{
		echo "# Live run: $driver"
		echo
		if [ "$dry" = 1 ]; then echo "**Dry run**: nothing was called; every answer is a stand-in."; echo; fi
		echo "- Revision: $revision"
		echo "- This box's egress narrowed before the key came in: $narrowed"
		echo "- Started: $started; ended: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "- Provider: \`${provider_id:-none}\`, pool: \`${pool_id:-none}\`, discobox: \`${box_id:-none}\` (named \`$name\`)"
		echo "- Egress: the pool allows \`$allow_host\` alone; \`$unlisted_host\` and \`1.1.1.1\` are unlisted"
		echo "- Evidence: \`$evidence\`"
		echo
		echo "| Result | Step | Detail |"
		echo "|---|---|---|"
		awk -F'\t' '{ gsub(/\|/, "\\|", $3); printf "| %s | %s | %s |\n", $1, $2, $3 }' "$results"
		echo
		if [ "$failed" = 0 ] && [ "$code" = 0 ] && [ "$dry" = 1 ]; then
			echo "**The dry run went through every step**; nothing was observed."
		elif [ "$failed" = 0 ] && [ "$code" = 0 ]; then
			echo "**Every check passed, and nothing is left.**"
		else
			echo "**$failed step(s) did not pass** (FAIL, or CHECK for a person to judge)."
		fi
		if grep -qE '^FAIL	left-[a-z]+-scope	' "$results"; then
			echo "Something was left under the run's scopes; see the left- rows."
		fi
		if grep -qE '^FAIL	left-[a-z]+-scope-listed	' "$results"; then
			echo "disco-vm could not list all of the run's scopes, so what is left there is not known; see the -listed rows."
		fi
	} | tee "$evidence/report.md"
	rm -rf "$work"
	if [ "$failed" != 0 ] && [ "$code" = 0 ]; then code=1; fi
	exit "$code"
}
trap finish EXIT
trap 'exit 130' INT TERM

# --- the run -----------------------------------------------------------

echo "--- preflight" >&2
# A dry run calls nothing out, so it needs only what reads the answers.
tools=(jq awk)
[ "$dry" = 1 ] || tools+=("$discobox" "$discovm" curl timeout)
for cli in "${tools[@]}"; do
	command -v "$cli" >/dev/null 2>&1 || { fail preflight "$cli is not on PATH"; exit 1; }
done
if [ "$dry" = 0 ] && [ -z "${!key_var:-}" ]; then
	fail preflight "\$$key_var is empty: hand the key over as a raw binding in that variable"
	exit 1
fi
# The server the CLI will send the key to: --server, else $DISCOBOX_SERVER,
# else its local default. The gate is the outer server's.
server=${DISCOBOX_SERVER:-}
for ((i = 0; i < ${#dc_flags[@]}; i++)); do
	case ${dc_flags[i]} in
	--server) server=${dc_flags[i + 1]:-} ;;
	--server=*) server=${dc_flags[i]#*=} ;;
	esac
done
case $dry:$server in
1:*discobox.internal*) note "the server is $server, this box's gate: a live run refuses it" ;;
0:*discobox.internal*)
	fail preflight "the server is $server, this box's gate to the outer server, which must never get the key: pass the box's own with --server, or unset DISCOBOX_SERVER"
	exit 1 ;;
esac
if ! dcout out 0 '[]' -o json admin provider list; then
	fail preflight "the server does not answer: $err"
	exit 1
fi
pass preflight "tools on PATH, \$$key_var set, the server ${server:-local} answers"

echo "--- provider" >&2
# The provider's config, on the driver's own provider type (ADR
# 26-11-11-080 §§1–2): its sealed key as driver.apiKey, with no
# deliverProviderKey, so the key never goes on a pool's machine (the plan's
# Q2).
config=$work/provider.json
if [ "$dry" = 1 ]; then
	jq -n --arg var "$key_var" '{driver: {apiKey: ("<$" + $var + ">")}}' >"$config"
	note "config: $(jq -c . "$config")"
else
	KEY_VAR=$key_var jq -n '{driver: {apiKey: $ENV[$ENV.KEY_VAR]}}' >"$config"
fi
if ! dcout out 0 '{"id":"prov_dryrun"}' -o json admin provider create --type "$driver" --name "$name" --config "@$config"; then
	rm -f "$config"
	fail provider "could not create: $err"
	exit 1
fi
rm -f "$config"
provider_id=$(field "$out" '.id // empty')
[ -n "$provider_id" ] || { fail provider "create printed no id: $out $err"; exit 1; }
pass provider "created $provider_id"

echo "--- pool" >&2
if ! dcout out 0 '{"id":"pool_dryrun"}' -o json admin pool create "$name" --provider "$provider_id"; then
	fail pool "could not create: $err"
	exit 1
fi
pool_id=$(field "$out" '.id // empty')
[ -n "$pool_id" ] || { fail pool "create printed no id: $out $err"; exit 1; }
# Every discobox of the pool may reach the one allowed host, and the
# harness's own (its model API), and nothing else.
if ! dcout out 0 '' admin pool update "$pool_id" --egress-deny-by-default --egress-allow "$allow_host"; then
	fail pool "could not restrict egress: $err"
	exit 1
fi
pass pool "created $pool_id, egress restricted to $allow_host"

waited=0
while :; do
	if ! dcout out 0 '{"state":"active","ready":true,"schedulable":true}' -o json admin pool get "$pool_id"; then
		fail pool-ready "could not read the pool: $err"
		exit 1
	fi
	state=$(field "$out" '"\(.state // "unknown") ready=\(.ready // false) schedulable=\(.schedulable // false)"')
	case $state in
	"active ready=true schedulable=true") break ;;
	failed*) fail pool-ready "failed: $(field "$out" '.errorMessage // "no message"')"; exit 1 ;;
	esac
	if [ "$waited" -ge "$pool_wait" ]; then
		fail pool-ready "not ready after $pool_timeout: $state, $(field "$out" '.errorMessage // ""')"
		exit 1
	fi
	pause 15
	waited=$((waited + 15))
done
pass pool-ready "active after ${waited}s"

echo "--- discobox" >&2
# The prompt asks for something its own text does not hold, so finding the
# answer on the screen shows the harness reached its model.
want=$(printf '%s' "$nonce" | awk '{ s = ""; for (i = length($0); i > 0; i--) s = s substr($0, i, 1); print s }')
prompt="Reply with the string $nonce written backwards, and nothing else. Do not run any tool."
run_args=(run --no-source --pool "$pool_id" -d -p "$prompt")
[ -z "$harness" ] || run_args+=(-H "$harness")
# Its progress goes to stderr, apart from the JSON on stdout.
if ! dcout out 0 '{"id":"sbx_dryrun"}' -o json "${run_args[@]}"; then
	fail discobox "could not create: $err"
	exit 1
fi
box_id=$(field "$out" '.id // empty')
[ -n "$box_id" ] || { fail discobox "run printed no id: $out $err"; exit 1; }
pass discobox "created $box_id"

if ! dcout out 0 'finished' wait "$box_id" --until finished --until blocked --for "$harness_timeout"; then
	fail harness "did not finish within $harness_timeout: $out $err"
else
	dcout screen 0 "$want" capture "$box_id" || true
	case $out in
	*blocked*) fail harness "blocked instead of answering: $out" ;;
	*)
		if printf '%s' "$screen" | grep -qF "$want"; then
			pass harness "answered $want"
		else
			fail harness "finished, but its screen does not show $want: $(printf '%s' "$screen" | tail -5)"
		fi ;;
	esac
fi

# Attaching is the stream a person gets: its first screen is enough. With no
# terminal it is raw; stdin stays open for a while, so it does not detach at
# once, and the timeout leaves the discobox running.
attached=$(ext 0 'dry run screen' timeout $((attach_wait + 10)) "$discobox" ${dc_flags[@]+"${dc_flags[@]}"} attach --raw "$box_id" < <(sleep "$attach_wait") 2>"$work/attach.err" || true)
printf '%s' "$attached" >"$evidence/attach.raw"
if [ -n "$attached" ]; then
	pass attach "the primary terminal streamed $(printf '%s' "$attached" | wc -c | tr -d ' ') bytes"
else
	fail attach "attach streamed nothing: $(tail -c 300 "$work/attach.err")"
fi

echo "--- egress" >&2
# One exec runs every probe from inside the discobox, as its user, with its
# login environment (the proxy's variables). Each prints
# NAME HTTP_CODE CONNECT_CODE CURL_EXIT TIME_CONNECT FIRST_LINE_OF_BODY.
# Through the proxy a refused host is asked for over plain HTTP, where the
# proxy answers 403 "blocked by proxy"; a refused CONNECT is only a closed
# connection (proxy/DESIGN.md), which a network failure looks like too.
# Around it, time_connect stays 0 only when no connection was made at all:
# curl's own error on a connection the fence let through (a TLS failure, an
# empty reply) is not a refusal.
probe=$(cat <<'EOF'
command -v curl >/dev/null 2>&1 || { echo "no-curl"; exit 0; }
probe() {
	name=$1; shift
	body=$(mktemp)
	codes=$(curl -sS -o "$body" -w '%{http_code} %{http_connect} %{time_connect}' --max-time 20 "$@" 2>/dev/null)
	exit=$?
	set -- ${codes:-000 000 0}
	printf '%s %s %s %s %s %s\n' "$name" "$1" "$2" "$exit" "$3" "$(head -c 200 "$body" | head -n 1 | tr -cd '[:print:]')"
	rm -f "$body"
}
printf 'proxy-env %s\n' "${HTTPS_PROXY:-${https_proxy:-none}}"
probe allowed-proxy "https://$1/"
probe unlisted-proxy "http://$2/"
probe ip-proxy "http://1.1.1.1/"
probe unlisted-direct --noproxy '*' "https://$2/"
probe ip-direct --noproxy '*' "https://1.1.1.1/"
EOF
)
canned="proxy-env http://discobox-pool-proxy:3128
allowed-proxy 200 200 0 0.012 <!doctype html>
unlisted-proxy 403 000 0 0.001 blocked by proxy
ip-proxy 403 000 0 0.001 blocked by proxy
unlisted-direct 000 000 6 0.000000
ip-direct 000 000 28 0.000000"
if ! dcout answers 0 "$canned" exec "$box_id" -- sh -lc "$probe" probe "$allow_host" "$unlisted_host"; then
	fail egress "the probes did not run: $answers $err"
elif [ "$answers" = no-curl ]; then
	fail egress "the discobox has no curl to probe with"
else
	printf '%s\n' "$answers" >"$evidence/egress.txt"
	line() { printf '%s\n' "$answers" | awk -v n="$1" '$1 == n'; }
	proxy_env=$(line proxy-env | awk '{ print $2 }')
	case $proxy_env in ''|none) note "the discobox has no HTTPS_PROXY; the proxy probes below fail" ;; esac
	# An allowed host: the pool's proxy answers with the origin's response.
	read -r _ code connect status _ body <<<"$(line allowed-proxy)"
	case ${code:-000}:$body in
	000:*|*:"blocked by proxy"*) fail egress-allowed "$allow_host did not answer through the proxy: HTTP ${code:-none}, CONNECT $connect, curl $status: $body" ;;
	*) pass egress-allowed "$allow_host answered $code through the proxy ($proxy_env)" ;;
	esac
	# Through the proxy, a host no level lists is refused there.
	for p in unlisted-proxy:"$unlisted_host" ip-proxy:1.1.1.1; do
		read -r _ code connect status _ body <<<"$(line "${p%%:*}")"
		case ${code:-}:$body in
		403:"blocked by proxy"*) pass "egress-${p%%:*}" "the proxy refused ${p#*:}: $body" ;;
		*) fail "egress-${p%%:*}" "${p#*:} through the proxy: HTTP ${code:-none}, curl $status: $body" ;;
		esac
	done
	# Around the proxy nothing is reached at all: the fence drops it.
	for p in unlisted-direct:"$unlisted_host" ip-direct:1.1.1.1; do
		read -r _ code connect status tconnect body <<<"$(line "${p%%:*}")"
		if [ "${code:-}" = 000 ] && [ -n "${status:-}" ] && [ "$status" != 0 ] && awk -v t="${tconnect:-x}" 'BEGIN { exit !(t ~ /^[0.]+$/) }'; then
			pass "egress-${p%%:*}" "${p#*:} is not reached around the proxy (curl $status, no connection)"
		else
			fail "egress-${p%%:*}" "${p#*:} around the proxy: HTTP ${code:-none}, curl ${status:-none}, connected after ${tconnect:-?}s: $body"
		fi
	done
fi

echo "--- public URL" >&2
# The machine's public URL, as the driver gives it, probed from here: outside
# the pool. Nothing the guest runs may answer there (the plan's B11), since
# the guest's root owns every port the URL could forward to. Only an answer
# observed decides it: a probe that reached nothing proves nothing, and is
# for a person to read.
machine=$(scope "$box_id")
if ! url=$(dvm 0 "https://$machine.dryrun.invalid" "$(scope "$pool_id")" url "$machine" 2>&1); then
	check public-url "the driver gives $machine no URL it could read: $url"
elif [ -z "$url" ]; then
	pass public-url "the driver gives $machine no public URL"
else
	headers=$evidence/public.headers
	: >"$headers"
	code=$(ext 0 '404' curl -sS -o "$evidence/public.body" -D "$headers" -w '%{http_code}' --max-time 20 "$url/discobox-live-run-$nonce" 2>"$work/public.err" || true)
	code=${code:-000}
	body=$(head -c 300 "$evidence/public.body" 2>/dev/null || true)
	server=$(awk -F': *' 'tolower($1) == "server" { print $2 }' "$headers")
	case $code in
	000) check public-url "$url was not reached ($(tail -c 300 "$work/public.err")); nothing is known of what answers there" ;;
	2??|401) fail public-url "$url answers $code, as the guest's service would: server '$server': $body" ;;
	*)
		# Go's TLS server answers plain HTTP with this: the request reached
		# the guest's own listener.
		case $body in
		*"HTTP request to an HTTPS server"*) fail public-url "$url reaches the guest's listener: HTTP $code: $body" ;;
		*) check public-url "$url answers HTTP $code (server '$server'): $body; read it: the provider's own refusal passes, anything from the guest fails" ;;
		esac ;;
	esac
fi
