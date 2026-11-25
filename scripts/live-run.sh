#!/usr/bin/env bash
# A repeatable live run of a remote driver, end to end: make a
# provider and a pool on DRIVER, start a discobox on it running a harness,
# attach to it, probe its egress and its machine's ingress URL (on boxd, its
# closed default proxy too), keep the evidence, delete everything it made,
# check that nothing is left, and report.
#
# Usage: scripts/live-run.sh --driver DRIVER [flags] [discobox flags...]
#
#   --driver DRIVER     boxd or archil (required)
#   --egress-narrowed yes|no
#                       whether this box's egress was narrowed to the plan's
#                       hosts before the key came in; the report says which
#                       (required, except on a dry run)
#   --region REGION    the region to exercise: on boxd fra or nyc (default:
#                       fra), on archil aws-us-east-1, aws-us-west-2 or
#                       aws-eu-west-1 (default: aws-us-east-1)
#   --harness SLUG      the harness the discobox runs (default: claude-code;
#                       a fresh server's project has no default harness)
#   --allow HOST        the one host the discobox's own egress allows, which
#                       the proxy must answer for (default: example.com)
#   --unlisted HOST     a host no level allows, which must not answer
#                       (default: example.net)
#   --pool-timeout DUR  how long the pool may take to come up, the discobox to
#                       finish provisioning, and anything deleted to go
#                       (default: 20m)
#   --harness-timeout DUR
#                       how long the harness may take to answer (default: 15m)
#   --egress-timeout DUR
#                       how long the discobox's narrowed egress may take to
#                       reach its proxy before it is judged (default: 5m)
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
# Scheduled and release validation runs use a fresh box on a reviewed,
# pinned commit. The manager binds dedicated test keys to their own hosts.
# The provider key is supplied in the variable disco-vm reads for the
# driver (BOXD_API_KEY or ARCHIL_API_KEY). Prepare the harness credential
# as a project secret on the run's server before calling this script.
#
# Pass the box's own server with --server unix:///path/to/socket or set
# DISCOBOX_SERVER to that socket URL. The script requires a Unix socket. The provider config is read from a private
# temporary file so the key is never on the command line.
#
# A check passes only on what it observed. One whose probe could not reach
# its target (the ingress URL from a box whose egress does not name its host,
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
region=
narrowed=
harness=claude-code
allow_host=example.com
unlisted_host=example.net
pool_timeout=20m
harness_timeout=15m
egress_timeout=5m
attach_for=10s
evidence=
dry=0
discobox=${DISCOBOX:-discobox}
discovm=${DISCO_VM:-disco-vm}
dc_flags=()

usage() { awk 'NR > 1 && !/^#/ { exit } NR > 1' "$0" | sed 's/^# \{0,1\}//'; }
need() { [ "$#" -ge 2 ] || { echo "$1 needs a value" >&2; exit 2; }; }
while [ "$#" -gt 0 ]; do
	case $1 in
	--driver) need "$@"; driver=$2; shift 2 ;;
	--driver=*) driver=${1#*=}; shift ;;
	--egress-narrowed) need "$@"; narrowed=$2; shift 2 ;;
	--egress-narrowed=*) narrowed=${1#*=}; shift ;;
	--region) need "$@"; region=$2; shift 2 ;;
	--region=*) region=${1#*=}; shift ;;
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
	--egress-timeout) need "$@"; egress_timeout=$2; shift 2 ;;
	--egress-timeout=*) egress_timeout=${1#*=}; shift ;;
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
# serves the provider's config and disco-vm's listing alike. Both drivers
# require a region, in the provider's config and in disco-vm's environment.
case $driver in
boxd)
	key_var=BOXD_API_KEY
	region=${region:-fra}
	case $region in
	fra) proxy_zone=boxd.sh; other_region=nyc ;;
	nyc) proxy_zone=nyc.boxd.sh; other_region=fra ;;
	*) echo "--region takes fra or nyc" >&2; exit 2 ;;
	esac ;;
archil)
	key_var=ARCHIL_API_KEY
	region=${region:-aws-us-east-1}
	# An Archil key is refused (401) at another region's control plane
	# (observed 2026-11-22 in both other regions), so no other region is
	# read: what the key cannot reach it cannot have made machines in either.
	other_region=
	case $region in
	aws-us-east-1|aws-us-west-2|aws-eu-west-1) ;;
	*) echo "--region takes aws-us-east-1, aws-us-west-2 or aws-eu-west-1 on archil" >&2; exit 2 ;;
	esac ;;
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
egress_wait=$(seconds "$egress_timeout")
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
# provision_failure names what failed in a box answer's provisioning: the
# document's own error (readiness itself), then each failed feature. Empty
# when it names nothing.
provision_failure() {
	field "$1" '.runtime.agentStatus.provision // {} | [(.error // empty), (.features // {} | to_entries[] | select(.value.state == "failed") | "\(.key) (\(.value.error // "no error"))")] | join("; ")'
}
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
dvm() { local status=$1 canned=$2 scope=$3; shift 3; BOXD_REGION=$region ARCHIL_REGION=$region ext "$status" "$canned" "$discovm" --driver "$driver" --pool "$scope" "$@"; }
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
second_box_id=
test_secret_id=
out=
screen=
answers=

# --- evidence, cleanup and report --------------------------------------

# keep NAME ARGS... saves what a discobox call prints, output and errors, under
# the evidence directory before cleanup runs.
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
	if [ -n "$second_box_id" ]; then
		keep second-discobox.json -o json admin box get "$second_box_id"
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
	if [ -z "$prefix" ]; then
		: # No pool was created, so no key can be identified as ours.
	elif out=$(dvm 0 '[]' "$scope" key ls --json 2>&1); then
		keys=$(issued "$out" "$prefix")
	elif ! printf '%s' "$out" | grep -qiE 'PermissionDenied|permission denied|interactive session|not supported'; then
		unlisted="$unlisted keys ($out)"
	fi
	if [ -n "$unlisted" ]; then
		fail "$step-listed" "disco-vm could not list scope $scope's $unlisted"
	fi
	if [ -z "$machines$caches$keys" ]; then
		[ -n "$unlisted" ] || pass "$step" "nothing left in scope $scope${region:+ in $region}"
		return
	fi
	fail "$step" "left in scope $scope: machines [$(words "$machines")] caches [$(words "$caches")] keys [$(words "$keys")]; removing them"
	local n
	for n in $keys; do dvm 0 '' "$scope" key revoke "$n" >&2 || note "could not revoke key $n"; done
	for n in $machines; do dvm 0 '' "$scope" rm --force "$n" >&2 || note "could not remove $n"; done
	for n in $caches; do dvm 0 '' "$scope" cache rm "$n" >&2 || note "could not remove cache $n"; done
	local remaining
	if ! remaining=$(dvm 0 'ID  NAME  FROM  OS  STATE  CREATED' "$scope" ls -a 2>&1); then
		fail "$step-removed" "could not read machines after removal"
		return
	fi
	machines=$(names "$remaining")
	if ! remaining=$(dvm 0 '[]' "$scope" cache ls --json 2>&1); then
		fail "$step-removed" "could not read caches after removal"
		return
	fi
	caches=$(field "$remaining" '.[]?.key // empty')
	if [ -n "$keys" ]; then
		if ! remaining=$(dvm 0 '[]' "$scope" key ls --json 2>&1); then
			fail "$step-removed" "could not read issued keys after removal"
			return
		fi
		keys=$(issued "$remaining" "$prefix")
	fi
	if [ -n "$machines$caches$keys" ]; then
		fail "$step-removed" "still left after removing: machines [$(words "$machines")] caches [$(words "$caches")] keys [$(words "$keys")]"
	else
		pass "$step-removed" "removed what was left in scope $scope"
	fi
}

cleanup() {
	set +e
	collect
	echo "--- cleanup" >&2
	if [ -n "$test_secret_id" ]; then
		if dc 0 '' secret rm "$test_secret_id" >&2; then
			pass cleanup-test-secret "removed the run's test secret"
		else
			fail cleanup-test-secret "could not remove the run's test secret"
		fi
	fi
	if [ -n "$second_box_id" ]; then
		if dc 0 '' admin box purge "$second_box_id" >&2; then
			pass cleanup-second-discobox "purged $second_box_id"
		else
			fail cleanup-second-discobox "could not purge $second_box_id"
		fi
	fi
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
	# Each region is its own endpoint, so a machine made in the wrong one is
	# found only there, on a driver whose key reaches another region.
	if [ -n "$other_region" ]; then
		if [ -n "$pool_id" ]; then region=$other_region leftovers "left-pool-scope-$other_region" "$(scope "$pool_id")" ""; fi
		if [ -n "$provider_id" ]; then region=$other_region leftovers "left-provider-scope-$other_region" "$(scope "$provider_id")" ""; fi
	fi
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
		echo "- Egress: the discobox allows \`$allow_host\` alone; \`$unlisted_host\` and \`1.1.1.1\` are unlisted"
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
		if grep -qE '^FAIL	left-[a-z]+-scope(-[a-z]+)?	left in scope ' "$results"; then
			echo "Something was left under the run's scopes; see the left- rows."
		fi
		if grep -qE '^FAIL	left-[a-z]+-scope(-[a-z]+)?-listed	' "$results"; then
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
1:*|0:unix:///*) ;;
*)
	fail preflight "the server is ${server:-unset}; live validation requires --server unix:///path/to/socket for the box's own server"
	exit 1 ;;
esac
if ! dcout out 0 '[]' -o json admin provider list; then
	fail preflight "the server does not answer: $err"
	exit 1
fi
pass preflight "tools on PATH, \$$key_var set, the server ${server:-local} answers"

if [ -n "$harness" ]; then
	if ! dcout out 0 '{}' -o json admin harness get "$harness"; then
		fail preflight "could not read harness $harness"
		exit 1
	fi
fi
if [ "$harness" = claude-code ]; then
	if ! dcout out 0 '{"boundSecrets":[{"use":true,"envVar":"ANTHROPIC_API_KEY","routes":[[{"hosts":["api.anthropic.com"]}]]}]}' -o json secret bound --harness "$harness" ||
		! printf '%s' "$out" | jq -e '
		[.boundSecrets[]? | select(.use and (.notApplied == null) and (.envVar == "ANTHROPIC_API_KEY" or .envVar == "CLAUDE_CODE_OAUTH_TOKEN"))]
		| length > 0 and all(.[]; .envVar as $env
			| any(.routes[]?;
				[.[] | select(.blocked != true) | .hosts[]] as $hosts
				| ($hosts | index("api.anthropic.com")) != null
				and all($hosts[]; . == "api.anthropic.com" or
					($env == "CLAUDE_CODE_OAUTH_TOKEN" and (. == "claude.ai" or . == "mcp-proxy.anthropic.com"))))
		)
		' >/dev/null; then
		fail preflight "Claude Code has no bound Anthropic credential limited to the harness's hosts on the run's server"
		exit 1
	fi
	pass preflight "the run's server has a host-scoped Claude Code credential binding"
fi

echo "--- provider" >&2
# The provider's config, on the driver's own provider type (ADR
# 26-11-11-080 §§1–2): its sealed key as driver.apiKey, with no
# deliverProviderKey, so the key never goes on a pool's machine (the plan's
# Q2).
config=$work/provider.json
if [ "$dry" = 1 ]; then
	jq -n --arg var "$key_var" --arg region "$region" '{driver: ({apiKey: ("<$" + $var + ">")} + (if $region == "" then {} else {region: $region} end))}' >"$config"
	note "config: $(jq -c . "$config")"
else
	KEY_VAR=$key_var jq -n --arg region "$region" '{driver: ({apiKey: $ENV[$ENV.KEY_VAR]} + (if $region == "" then {} else {region: $region} end))}' >"$config"
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

# A create checks only the config, so whether the server reaches the
# provider's service (its proxy environment, its egress) is asked here,
# before a pool is made: a pool on a provider the server cannot reach only
# fails later, as a bootstrap that stopped making progress. The cleanup
# removes the provider.
if ! dcout out 0 '{"outcome":"ok"}' -o json admin provider check "$provider_id"; then
	message=$(field "$out" '.message // empty')
	fail preflight "the server cannot use provider $provider_id ($(field "$out" '.outcome // "no answer"')): ${message:-$err}"
	exit 1
fi
pass preflight "the server reaches the $driver provider $provider_id"

echo "--- pool" >&2
if ! dcout out 0 '{"id":"pool_dryrun"}' -o json admin pool create "$name" --provider "$provider_id"; then
	fail pool "could not create: $err"
	exit 1
fi
pool_id=$(field "$out" '.id // empty')
[ -n "$pool_id" ] || { fail pool "create printed no id: $out $err"; exit 1; }
# The pool's egress level is left open: it holds its plan's snapshot build
# (every level but the discobox's own, store.ProvisionEgress), and a box
# started from the base provisions under every level, so a pool narrowed to
# the allowed host could provision nothing. The discobox's own level is
# narrowed instead, once it has provisioned (--- egress).
pass pool "created $pool_id"

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

echo "--- test secret" >&2
# Bind a harmless unique value with no exchange routes. Its sentinel has
# a distinct, known shape so the guest can check it without printing either
# the sentinel or the value.
test_value=live-run-value-$nonce
secret_config=$work/test-secret.json
jq -n --arg token "$test_value" '{token: $token}' >"$secret_config"
# A secret names where it may be sent; this one's only route is a host no
# resolver answers (RFC 2606), so its value goes nowhere.
if ! dcout out 0 '{"id":"sec_dryrun"}' -o json secret create --name "$name" --type token --format 'live-run-sentinel-{hex:32}' --route live-run.invalid --value-json "@$secret_config"; then
	fail sentinels "could not create the test secret: $err"
	exit 1
fi
rm -f "$secret_config"
test_secret_id=$(field "$out" '.id // empty')
if [ -z "$test_secret_id" ] || ! dcout out 0 '' secret bind "$test_secret_id" --project-wide --as DISCOBOX_LIVE_RUN_TEST; then
	fail sentinels "could not bind the test secret"
	exit 1
fi

probe_sentinel() {
	local id=$1 step=$2 waited=0
	# A runtime config can arrive after the box first reports running.
	while :; do
		# shellcheck disable=SC2016 # This command expands its arguments in the sandbox.
		if dc 0 '' exec "$id" -- sh -c '
			if env | grep -Fq -- "$1"; then exit 1; fi
			sudo -n jq -e --arg raw "$1" "$2" /run/discobox/secrets/secrets.json
		' probe "$test_value" '
			(.DISCOBOX_LIVE_RUN_TEST | strings | test("^live-run-sentinel-[0-9a-f]{32}$")) and
			all(.. | strings; contains($raw) | not)
		' >/dev/null 2>&1; then
			pass "$step" "$id has the expected test sentinel after ${waited}s"
			return
		fi
		if [ "$waited" -ge "$pool_wait" ]; then
			fail "$step" "$id did not expose the expected test sentinel within $pool_timeout"
			return
		fi
		pause 10
		waited=$((waited + 10))
	done
}
probe_pool_sentinel() {
	local id=$1
	# shellcheck disable=SC2016 # The arguments expand on the pool machine.
	if dvm 0 '' "$(scope "$provider_id")" exec "$(scope "$pool_id")" sh -c '
		raw=$3 query=$4
		set -- /var/lib/discobox/projects/*/pools/"$1"/sandboxes/"$2"/runtime-config.json
		[ "$#" = 1 ] && jq -e --arg raw "$raw" "$query" "$1"
	' probe "$pool_id" "$id" "$test_value" '
		(.document.secretEnv.DISCOBOX_LIVE_RUN_TEST | strings | test("^live-run-sentinel-[0-9a-f]{32}$")) and
		all(.. | strings; contains($raw) | not)
	' >/dev/null 2>&1; then
		pass pool-sentinels "the pool recorded a sentinel for $id"
	else
		fail pool-sentinels "the pool's runtime config for $id did not show the expected sentinel"
	fi
}
if [ "$driver" = boxd ]; then
	if dvm 0 '' "$(scope "$provider_id")" exec "$(scope "$pool_id")" sh -c '
		test ! -e /etc/discobox/pool-agent/provisioning-key.json &&
		jq -e ".driverConfig.inMachine == true and (.driverConfig | has(\"apiKey\") | not)" /etc/discobox/pool-agent/bootstrap.json
	' >/dev/null 2>&1; then
		pass pool-credential "boxd pool uses in-machine authentication and has no provider key in its bootstrap or provisioning key file"
	else
		fail pool-credential "could not confirm in-machine authentication without a provider key in the pool bootstrap"
	fi
fi

echo "--- discobox" >&2
# The prompt asks for something its own text does not hold, so finding the
# answer on the screen shows the harness reached its model.
want=$(printf '%s' "$nonce" | awk '{ s = ""; for (i = length($0); i > 0; i--) s = s substr($0, i, 1); print s }')
prompt="Reply with the string $nonce written backwards, and nothing else. Do not run any tool."
run_args=(run --no-source --pool "$pool_id" -d)
[ "$harness" = claude-code ] || run_args+=(-p "$prompt")
[ -z "$harness" ] || run_args+=(-H "$harness")
# Its progress goes to stderr, apart from the JSON on stdout.
if ! dcout out 0 '{"id":"sbx_dryrun"}' -o json "${run_args[@]}"; then
	fail discobox "could not create: $err"
	exit 1
fi
box_id=$(field "$out" '.id // empty')
[ -n "$box_id" ] || { fail discobox "run printed no id: $out $err"; exit 1; }
pass discobox "created $box_id"

# Provisioning goes on past readiness (a feature marked async), and a box
# started from the base provisions under its own level too: it is narrowed
# only once nothing more will be provisioned, so no feature is cut off by it.
# What failed is reported; a box whose features could not be fetched is the
# failure this run is meant to see.
waited=0
while :; do
	if ! dcout out 0 '{"id":"sbx_dryrun","runtime":{"agentStatus":{"provision":{"state":"ready"}}}}' -o json admin box get "$box_id"; then
		fail provision "could not read the discobox: $err"
		break
	fi
	provisioned=$(field "$out" '.runtime.agentStatus.provision.state // "unknown"')
	case $provisioned in
	ready) pass provision "every feature is ready after ${waited}s"; break ;;
	failed)
		# Failed is a feature that failed, or readiness itself, which only
		# the document's own error names.
		why=$(provision_failure "$out")
		fail provision "failed: ${why:-no error given}"
		break ;;
	esac
	if [ "$waited" -ge "$pool_wait" ]; then
		fail provision "still $provisioned after $pool_timeout"
		break
	fi
	pause 10
	waited=$((waited + 10))
done

# Attaching is the stream a person gets once the discobox is up: its first
# screen is enough. Attached while it still provisions (minutes on archil),
# it shows only progress. With no terminal it is raw; stdin stays open for a
# while, so it does not detach at once, and the timeout leaves the discobox
# running.
attached=$(ext 0 'dry run screen' timeout $((attach_wait + 10)) "$discobox" ${dc_flags[@]+"${dc_flags[@]}"} attach --raw "$box_id" < <(sleep "$attach_wait") 2>"$work/attach.err" || true)
printf '%s' "$attached" >"$evidence/attach.raw"
if [ -n "$attached" ]; then
	pass attach "the primary terminal streamed $(printf '%s' "$attached" | wc -c | tr -d ' ') bytes"
else
	fail attach "attach streamed nothing: $(tail -c 300 "$work/attach.err")"
fi

if [ "$harness" = claude-code ]; then
	harness_started=$SECONDS
	rc=0
	answer=$(ext 0 "$want" timeout -k 5s "$harness_timeout" "$discobox" ${dc_flags[@]+"${dc_flags[@]}"} exec "$box_id" -- claude -p "$prompt" </dev/null 2>"$work/harness.err") || rc=$?
	if [ "$rc" = 0 ] && printf '%s\n' "$answer" | grep -Fxq "$want"; then
		pass harness "Claude Code returned the expected answer after $((SECONDS - harness_started))s"
	elif [ "$rc" = 124 ]; then
		fail harness "Claude Code did not answer within $harness_timeout"
	else
		fail harness "Claude Code exited $rc after $((SECONDS - harness_started))s without the expected answer; it printed: $(printf '%s' "$answer" | tail -c 200) $(tail -c 300 "$work/harness.err")"
	fi
else
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
fi

probe_sentinel "$box_id" sentinels
probe_pool_sentinel "$box_id"

echo "--- egress" >&2
# The discobox's own level: it may reach the one allowed host, and the
# harness's own (its model API), and nothing else. It is set only now, so the
# harness above answered while the box was open: a box made with its level
# already narrowed (admin box create takes the flags, run does not) would
# provision under it, as the plan's first box does from the base, and fail.
box_narrowed=0
if dcout out 0 '' admin box update "$box_id" --egress-deny-by-default --egress-allow "$allow_host"; then
	box_narrowed=1
	pass egress-level "the discobox's own egress is restricted to $allow_host"
else
	fail egress-level "could not restrict the discobox's egress to $allow_host, so nothing is probed: $err"
fi
# One exec runs every probe from inside the discobox, as its user, with its
# login environment (the proxy's variables). Each prints
# NAME HTTP_CODE CONNECT_CODE CURL_EXIT TIME_CONNECT FIRST_LINE_OF_BODY.
# Through the proxy a refused host is asked for over plain HTTP, where the
# proxy answers 403 "blocked by proxy". A refused CONNECT is answered 403 as
# well (proxy/DESIGN.md), but curl reports that as a CONNECT failure rather
# than a body, so plain HTTP is the probe whose answer reads the same either way.
# Around it, a fence that drops the connection leaves time_connect 0. boxd's
# fence accepts it and then refuses: TLS closes at the ClientHello (curl 35),
# whatever the host. So 192.0.2.1 (TEST-NET-1, where no server answers) is
# probed around the proxy too: a connection there is the fence's own, and its
# refusal is what the fence's refusal of a real host looks like.
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
probe fence-direct --noproxy '*' "https://192.0.2.1/"
probe allowed-direct --noproxy '*' "https://$1/"
probe unlisted-direct --noproxy '*' "https://$2/"
probe ip-direct --noproxy '*' "https://1.1.1.1/"
EOF
)
canned="proxy-env http://discobox-pool-proxy:3128
allowed-proxy 200 200 0 0.012 <!doctype html>
unlisted-proxy 403 000 0 0.001 blocked by proxy
ip-proxy 403 000 0 0.001 blocked by proxy
fence-direct 000 000 28 0.000000
allowed-direct 000 000 6 0.000000
unlisted-direct 000 000 6 0.000000
ip-direct 000 000 28 0.000000"
# line NAME is the probe NAME's line of $answers.
line() { printf '%s\n' "$answers" | awk -v n="$1" '$1 == n'; }
# The discobox converges onto its level while running, so the probes run
# again until its proxy refuses the unlisted host, for at most
# --egress-timeout; the last answers are the ones judged.
waited=0
probed=0
while [ "$box_narrowed" = 1 ]; do
	probed=0
	dcout answers 0 "$canned" exec "$box_id" -- sh -lc "$probe" probe "$allow_host" "$unlisted_host" || break
	probed=1
	[ "$answers" != no-curl ] || break
	if [ "$(line unlisted-proxy | awk '{ print $2 }')" = 403 ]; then
		[ "$waited" = 0 ] || note "the narrowed level reached the discobox's proxy after ${waited}s"
		break
	fi
	if [ "$waited" -ge "$egress_wait" ]; then
		note "the proxy still answers for $unlisted_host $egress_timeout after the discobox was narrowed"
		break
	fi
	pause 5
	waited=$((waited + 5))
done
if [ "$box_narrowed" = 0 ]; then
	:
elif [ "$probed" = 0 ]; then
	fail egress "the probes did not run: $answers $err"
elif [ "$answers" = no-curl ]; then
	fail egress "the discobox has no curl to probe with"
else
	printf '%s\n' "$answers" >"$evidence/egress.txt"
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
	# Around the proxy nothing is reached at all. The fence drops the
	# connection (no time_connect), or accepts it and closes TLS at the
	# ClientHello (curl 35) as it does at 192.0.2.1, where only the fence can
	# accept: no handshake completed, so no origin answered. Any other
	# failure after a connection (a certificate, an empty reply, a reset, a
	# timeout) may be an origin's, and an answer is one.
	fence=no
	read -r _ code _ status tconnect _ <<<"$(line fence-direct)"
	if [ "${code:-}" = 000 ] && [ "${status:-}" = 35 ] && ! awk -v t="${tconnect:-x}" 'BEGIN { exit !(t ~ /^[0.]+$/) }'; then
		fence=yes
	fi
	for p in allowed-direct:"$allow_host" unlisted-direct:"$unlisted_host" ip-direct:1.1.1.1; do
		read -r _ code connect status tconnect body <<<"$(line "${p%%:*}")"
		if [ "${code:-}" != 000 ] || [ -z "${status:-}" ] || [ "$status" = 0 ]; then
			fail "egress-${p%%:*}" "${p#*:} answered around the proxy: HTTP ${code:-none}, curl ${status:-none}, connected after ${tconnect:-?}s: $body"
		elif awk -v t="${tconnect:-x}" 'BEGIN { exit !(t ~ /^[0.]+$/) }'; then
			pass "egress-${p%%:*}" "${p#*:} is not reached around the proxy (curl $status, no connection)"
		elif [ "$status" != 35 ]; then
			fail "egress-${p%%:*}" "${p#*:} around the proxy: HTTP $code, curl $status, connected after ${tconnect}s: $body"
		elif [ $fence = yes ]; then
			pass "egress-${p%%:*}" "${p#*:} is refused around the proxy before TLS completes (curl 35), as 192.0.2.1, where no server answers, is"
		else
			check "egress-${p%%:*}" "${p#*:} closed TLS around the proxy (curl 35, connected after ${tconnect}s), but 192.0.2.1 did not connect and refuse it the same way ($(line fence-direct)): read egress.txt; only the fence's refusal passes"
		fi
	done
fi

echo "--- ingress URL" >&2
# The machine's ingress URL, probed from here: outside the pool. Nothing the
# guest runs may answer there (the plan's B11), since the guest's root owns
# every port the URL could forward to. Only an answer observed decides it: a
# probe that reached nothing proves nothing, and is for a person to read.
machine=$(scope "$box_id")
# reach NAME URL CANNED probes URL from here, keeping what answered in the
# evidence as NAME.headers and NAME.body: the HTTP status in $code (000:
# nothing answered), what answered in $body and $server, and curl's error
# in $work/NAME.err.
code=
body=
server=
reach() {
	local step=$1 url=$2 headers=$evidence/$1.headers
	: >"$headers"
	code=$(ext 0 "$3" curl -sS -o "$evidence/$step.body" -D "$headers" -w '%{http_code}' --max-time 20 "$url/discobox-live-run-$nonce" 2>"$work/$step.err" || true)
	body=$(head -c 300 "$evidence/$step.body" 2>/dev/null || true)
	# curl keeps each header line's CR.
	server=$(awk -F': *' 'tolower($1) == "server" { print $2 }' "$headers" | tr -d '\r')
	code=${code:-000}
}
# guest STEP URL CODE fails STEP on an answer only the guest gives, and
# returns 1 for any other.
guest() {
	case $3 in
	2??|401) fail "$1" "$2 answers $3, as the guest's service would: server '$server': $body" ;;
	*)
		# Go's TLS server answers plain HTTP with this: the request reached
		# the guest's own listener.
		case $body in
		*"HTTP request to an HTTPS server"*) fail "$1" "$2 reaches the guest's listener: HTTP $3: $body" ;;
		*) return 1 ;;
		esac ;;
	esac
}

# The URL the driver reports. A sandbox machine is made with its ingress URL
# off, so a driver that can withhold one reports none, and disco-vm url
# refuses (machine.ErrUnsupported); a URL it does report is probed.
if ! url=$(dvm 1 "disco-vm: instance $machine has no ingress URL: machine: not supported by this driver" "$(scope "$pool_id")" url "$machine" 2>&1); then
	case $url in
	*"not supported by this driver"*) pass ingress-url "the driver reports no URL for $machine: $url" ;;
	*) check ingress-url "the driver gives $machine no URL it could read: $url" ;;
	esac
elif [ -z "$url" ]; then
	pass ingress-url "the driver reports no URL for $machine"
else
	reach ingress "$url" 404
	case $code in
	000) check ingress-url "$url was not reached ($(tail -c 300 "$work/ingress.err")); nothing is known of what answers there" ;;
	*) guest ingress-url "$url" "$code" ||
		check ingress-url "$url answers HTTP $code (server '$server'): $body; read it: the provider's own refusal passes, anything from the guest fails" ;;
	esac
fi

# What the driver stopped reporting is still there on boxd: every machine
# keeps its default proxy, at its name in its region's zone, which boxd
# cannot delete. For a machine whose ingress URL is off, disco-vm locks it to
# port 0, which no guest can listen on, and boxd answers 502 there (disco-vm
# docs/drivers/boxd.md, The ingress URL). That proxy is probed itself,
# whatever disco-vm url said: a check of only the URL the driver reports
# cannot see a proxy it stopped reporting. The machine is
# dvm-<pool>--<name> (the same file, Create), in the selected region's zone.
#
# boxd answers 502 too where a proxy left open forwards to a port nothing
# listens on, which it picks from the ports below (the first the guest
# listens on, else 8000). So while the probe runs, the guest listens on each
# of them it can bind, answering a marker, and says which of them
# something already listens on, on any address (busy, read from
# /proc/net/tcp and tcp6 before it binds anything). With 8000 ours and nothing else on a port
# ahead of it (80, 443, 8080), an open proxy forwards to our listener, whose
# marker comes back: only a proxy that forwards nowhere still answers 502. A
# service already ahead of 8000 may answer 502 itself, or boxd for it, so a
# 502 then is for a person to read. 80 and 443 need root, which the box's
# user may not have without a password: unbound and not busy, nothing is
# there for a proxy to pick. disco-vm turns boxd's bot protection off on the
# proxy it closes, or boxd would answer every non-browser client with a
# challenge (x-boxd-proxy: challenge) in place of its 502: that answer names
# a disco-vm pin from before it did.
if [ "$driver" = boxd ]; then
	proxy=https://dvm-$(scope "$pool_id")--$machine.$proxy_zone
	marker="discobox-live-run-listener"
	listen=$(cat <<'EOF'
command -v python3 >/dev/null 2>&1 || { echo "no-python3"; exit 0; }
run=; ! sudo -n true 2>/dev/null || run="sudo -n"
exec $run python3 -c '
import http.server, sys, threading, time
marker, secs = sys.argv[1], int(sys.argv[2])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = (marker + "\n").encode()
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
ports = (80, 443, 8080, 8000, 3000, 5000, 5173)
# What already listens, on any address, before anything is bound: state 0A
# in /proc/net/tcp and tcp6, which any user can read.
held, read = set(), False
for f in ("/proc/net/tcp", "/proc/net/tcp6"):
    try: rows = open(f).read().splitlines()[1:]
    except OSError: continue
    read = True
    for r in rows:
        c = r.split()
        if len(c) > 3 and c[3] == "0A": held.add(int(c[1].rsplit(":", 1)[1], 16))
# Not known is not free: what holds a port ahead of 8000 then is unknown.
busy = [str(p) for p in ports if p in held] if read else ["unknown"]
up = []
for p in ports:
    if p in held: continue
    try: s = http.server.ThreadingHTTPServer(("0.0.0.0", p), H)
    except OSError: continue
    threading.Thread(target=s.serve_forever, daemon=True).start(); up.append(str(p))
print("busy " + " ".join(busy), flush=True)
print("listening " + " ".join(up), flush=True)
time.sleep(secs)
' "$1" "$2"
EOF
)
	# It listens for 60s, long enough for the probe's 20s, and then ends on
	# its own; the purge in the cleanup ends it too.
	ext 0 "busy
listening 8080 8000 3000 5000 5173" "$discobox" ${dc_flags[@]+"${dc_flags[@]}"} exec "$box_id" -- sh -c "$listen" listen "$marker" 60 </dev/null >"$work/listen.out" 2>"$work/listen.err" &
	listener=$!
	# In dry runs pause returns immediately. Wait for the canned writer to finish.
	if [ "$dry" = 1 ]; then
		wait "$listener"
	fi
	for _ in $(seq 30); do
		grep -q '^listening\|^no-python3' "$work/listen.out" 2>/dev/null && break
		kill -0 "$listener" 2>/dev/null || break
		pause 1
	done
	listening=$(awk '$1 == "listening" { $1 = ""; print }' "$work/listen.out" | sed 's/^ *//')
	busy=$(awk '$1 == "busy" { $1 = ""; print }' "$work/listen.out" | sed 's/^ *//')
	cp "$work/listen.out" "$evidence/closed-proxy.listener" 2>/dev/null || true
	reach closed-proxy "$proxy" 502
	kill "$listener" 2>/dev/null || true
	wait "$listener" 2>/dev/null || true
	heard=
	case " $listening " in
	*" 8000 "*) heard="while the guest listened on $listening" ;;
	esac
	case " $busy " in
	*" 80 "*|*" 443 "*|*" 8080 "*|*" unknown "*) heard= ;;
	esac
	case $code:$body in
	000:*) check closed-proxy "$proxy was not reached ($(tail -c 300 "$work/closed-proxy.err")); nothing is known of what answers there" ;;
	*:*"$marker"*) fail closed-proxy "$proxy forwards to the guest: HTTP $code, its listener's marker came back: $body" ;;
	502:*)
		if guest closed-proxy "$proxy" "$code"; then :
		elif [ -n "$heard" ]; then
			pass closed-proxy "$proxy answers 502 $heard: forwarded nowhere, closed (server '$server'): $body"
		else
			check closed-proxy "$proxy answers 502, but the guest did not hold 8000 with nothing else ahead of it (listening on: ${listening:-none}; already held: ${busy:-none}; $(tr '\n' ' ' <"$work/listen.out" | head -c 200) $(tail -c 200 "$work/listen.err")), so the 502 does not tell a closed proxy from an open one"
		fi ;;
	*)
		proxied=$(awk -F': *' 'tolower($1) == "x-boxd-proxy" { print $2 }' "$evidence/closed-proxy.headers" | tr -d '\r')
		if guest closed-proxy "$proxy" "$code"; then :
		elif [ "$proxied" = challenge ]; then
			check closed-proxy "$proxy answers boxd's bot-protection challenge (HTTP $code, x-boxd-proxy: challenge), which hides whether it forwards to the guest: the disco-vm the pool agent was built with keeps bot protection on at a closed proxy, so pins one from before discobox-ai/vm#167 (#1216)"
		else
			check closed-proxy "$proxy answers HTTP $code (server '$server'): $body; read it: boxd's own refusal passes, anything from the guest fails"
		fi ;;
	esac
fi

{
	machine_scope=$(scope "$pool_id")
	if ! listed=$(dvm 0 "ID NAME FROM OS STATE CREATED
dry-id $machine - linux running now" "$machine_scope" ls -a); then
		fail region "could not list the sandbox scope in $region"
	elif ! names "$listed" | grep -Fxq "$machine"; then
		fail region "$machine was not listed in $region"
	elif [ -z "$other_region" ]; then
		pass region "$machine is listed in $region; no other region is read, since the $driver key reaches its own region alone"
	elif ! listed=$(region=$other_region dvm 0 'ID NAME FROM OS STATE CREATED' "$machine_scope" ls -a); then
		check region "could not list the sandbox scope in $other_region"
	elif names "$listed" | grep -Fxq "$machine"; then
		fail region "$machine is also listed in $other_region"
	else
		pass region "$machine is listed in $region and absent from $other_region"
	fi
}

echo "--- snapshot" >&2
# Creating the first box schedules its plan's snapshot on the pool. Wait
# for the pool to hold it before creating a second box with the same plan.
waited=0
snapshot=
while :; do
	if ! dcout out 0 '{"resources":{"snapshots":{"held":[{"key":"dry-snapshot"}]}}}' -o json admin pool get "$pool_id"; then
		fail snapshot "could not read the pool's snapshots: $err"
		break
	fi
	snapshot=$(field "$out" '.resources.snapshots.held[0].key // empty')
	if [ -n "$snapshot" ]; then
		pass snapshot "pool holds $snapshot after ${waited}s"
		break
	fi
	if [ "$waited" -ge "$pool_wait" ]; then
		fail snapshot "no held snapshot after $pool_timeout: $(field "$out" '.resources.snapshots.failed // [] | tojson')"
		break
	fi
	pause 10
	waited=$((waited + 10))
done
if [ -n "$snapshot" ]; then
	second_args=(run --no-source --pool "$pool_id" -d)
	[ -z "$harness" ] || second_args+=(-H "$harness")
	if dcout out 0 '{"id":"sbx_dryrun2"}' -o json "${second_args[@]}"; then
		second_box_id=$(field "$out" '.id // empty')
	fi
	if [ -z "$second_box_id" ]; then
		fail snapshot-start "could not create the second discobox: $err"
	else
		waited=0
		while :; do
			if ! dcout out 0 '{"runtime":{"state":"ready","runtimeState":"running","displayState":"running","startedFrom":{"from":"snapshot","snapshot":"dry-snapshot"}}}' -o json admin box get "$second_box_id"; then
				fail snapshot-start "could not read the second discobox: $err"
				break
			fi
			# displayState is the server's one composition of the box's
			# existence (.runtime.state) and power (.runtime.runtimeState),
			# which a client never combines itself: running once the box is up
			# and its required setup done, error once either failed.
			state=$(field "$out" '.runtime.displayState // "unknown"')
			if [ "$state" = running ]; then
				started_from=$(field "$out" '.runtime.startedFrom | select(.from == "snapshot") | .snapshot // empty')
				if [ "$started_from" = "$snapshot" ]; then
					pass snapshot-start "$second_box_id started from $snapshot after ${waited}s"
					probe_sentinel "$second_box_id" snapshot-sentinels
					probe_pool_sentinel "$second_box_id"
				else
					fail snapshot-start "$second_box_id started from ${started_from:-no snapshot}, expected $snapshot"
				fi
				break
			fi
			if [ "$state" = error ]; then
				# A box that is up reads error when its required setup
				# failed, which only its provisioning document names; any
				# other error is the one the server recorded.
				why=$(field "$out" '.runtime.errorMessage // empty')
				setup=$(provision_failure "$out")
				[ -z "$setup" ] || why=${why:+$why; }$setup
				fail snapshot-start "$second_box_id failed after ${waited}s: ${why:-no error given}"
				break
			fi
			if [ "$waited" -ge "$pool_wait" ]; then
				fail snapshot-start "$second_box_id is $state after ${waited}s"
				break
			fi
			pause 10
			waited=$((waited + 10))
		done
	fi
fi

echo "--- channel" >&2
if dcout out 0 'control-plane channel open' admin pool logs "$pool_id" --tail 0; then
	printf '%s\n' "$out" >"$evidence/channel.log"
	opens=$(printf '%s\n' "$out" | awk '/control-plane channel open/ { n++ } END { print n+0 }')
	cuts=$(printf '%s\n' "$out" | awk '/control-plane channel closed/ { n++ } END { print n+0 }')
	if [ "$opens" -eq 0 ]; then
		check channel "the pool journal records no control-plane channel opening"
	else
		pass channel "pool journal: $cuts cuts, $((opens - 1)) successful redials"
	fi
else
	check channel "could not read the pool journal: $err"
fi

if [ "$driver" = archil ]; then
	echo "--- Archil edge strips the port token (requires Archil rollout)" >&2
	step=archil-edge-strips-port-token
	scope_id=$(scope "$pool_id")
	# Ask for the same credentialed endpoint the pool uses. Never record the
	# credential or pass it in argv. A failed request cannot prove stripping.
	if service=$(dvm 0 $'https://live-run.archil.app\nX-Archil-Token: dry-run\n2099-01-01T00:00:00Z' "$scope_id" url --scope pool --credential "$machine" 2>/dev/null); then
		{
			IFS= read -r endpoint
			IFS= read -r header || true
		} <<<"$service"
		if [[ $endpoint =~ ^https://[a-zA-Z0-9.-]+\.archil\.app$ && $header =~ ^X-Archil-Token:\ [^[:space:]]+$ ]]; then
			# This disposable sandbox is finished with its normal agent. Run
			# the probe out of band so stopping the agent cannot cut off exec.
			# The listener returns only a nonce and a presence bit.
			probe=$(cat <<'PY'
from http.server import BaseHTTPRequestHandler, HTTPServer
import sys

class Listener(BaseHTTPRequestHandler):
    handled = False

    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.path != "/" + sys.argv[1]:
            self.send_error(404)
            return
        seen = "present" if "X-Archil-Token" in self.headers else "absent"
        self.send_response(200)
        self.end_headers()
        self.wfile.write(f"{sys.argv[1]} {seen}\n".encode())
        Listener.handled = True

with HTTPServer(("0.0.0.0", 7302), Listener) as server:
    print("ready", flush=True)
    while not Listener.handled:
        server.handle_request()
PY
)
			# shellcheck disable=SC2016 # Positional arguments expand in the guest.
			dvm 0 ready "$scope_id" exec --user root "$machine" sh -c '
				set -e
				trap "systemctl start discobox-sandbox-agent.service" EXIT
				systemctl stop discobox-sandbox-agent.service
				timeout -k 2s 60s python3 -u -c "$1" "$2"
			' probe "$probe" "$nonce" </dev/null >"$work/archil-listener.out" 2>"$work/archil-listener.err" &
			listener=$!
			for ((attempt = 0; attempt < 30; attempt++)); do
				if grep -qx ready "$work/archil-listener.out"; then break; fi
				kill -0 "$listener" 2>/dev/null || break
				pause 1
			done
			if [ "$dry" = 1 ] || grep -qx ready "$work/archil-listener.out"; then
				answer=$(printf '%s\n' "$header" | ext 0 "$nonce absent" curl -q --http1.1 --fail --silent --max-time 20 --max-filesize 256 --header @- "$endpoint/$nonce") || answer=
				case $answer in
				"$nonce absent") pass "$step" "authenticated request reached the guest without X-Archil-Token" ;;
				"$nonce present") fail "$step" "the guest received X-Archil-Token; this check requires Archil's header-stripping rollout" ;;
				*) check "$step" "no authenticated response from the guest listener; header stripping was not observed" ;;
				esac
			else
				check "$step" "the guest listener did not start"
			fi
			# The guest exits after its nonce request or the guest timeout.
			# Wait for its trap to restore the agent before teardown.
			wait "$listener" 2>/dev/null || true
		else
			check "$step" "disco-vm did not return an Archil HTTPS endpoint and port credential"
		fi
	else
		check "$step" "could not obtain the Archil port credential; disco-vm must support url --credential"
	fi
	unset service header
fi
