#!/usr/bin/env bash
# orchestrate.sh — the lead discobox's loop over the worker discoboxes it created.
#
# State lives in $ORCH_DIR (default ~/.local/state/orchestrate), which survives a
# wiped scratchpad: workers.tsv ("<issue>\t<discobox-id>\t<pr>", pr may be "-"),
# pool.tsv (triage mode: "<discobox-id>\t<issue>\t<triaging|done>"), verdicts.log,
# tried.txt (request IDs already answered), rebased.txt, approvals.log, kicks.log.
# A box is in one of workers.tsv (delivering) and pool.tsv (triaging) at a time.
#
# Use IDs are looked up by their descriptions on every call, so a renewed grant
# is picked up without editing anything. discobox-access always gets
# </dev/null: it shows the judge what a command reads on stdin, and in a loop it
# would read the loop's input.
set -uo pipefail

ORCH_DIR=${ORCH_DIR:-$HOME/.local/state/orchestrate}
REPO=${ORCH_REPO:-discobox-ai/discobox}
mkdir -p "$ORCH_DIR"
W=$ORCH_DIR/workers.tsv
P=$ORCH_DIR/pool.tsv
touch "$W" "$P" "$ORCH_DIR/tried.txt" "$ORCH_DIR/rebased.txt"

# use <credential-name> <regex>: the live use ID whose description matches.
use() {
	discobox-access list --json </dev/null 2>/dev/null |
		jq -r --arg c "$1" --arg re "$2" \
			'[.credentials[] | select(.name==$c) | .uses[] | select(.description|test($re))] | last | .useId // empty'
}
# need <use-id> <what>: a missing use is printed as MISSING USE (watch exits on it) and fails the caller.
need() { [ -n "$1" ] && return 0; echo "MISSING USE: no live use for $2; ask for it (SKILL.md §1)"; return 1; }
box()  { local u; u=$(use ai.discobox.sandbox '^discobox admin box ls'); need "$u" "box ls/get" || return 1; discobox-access run --use "$u" -- discobox admin box "$@" </dev/null; }
gh_()  { local u; u=$(use github 'pulls \(with query parameters\), repos/[^ ]+/pulls/<number>'); need "$u" "PR reads" || return 1; discobox-access run --use "$u" -- gh "$@" </dev/null; }

workers() { grep -v '^\s*$' "$W" || true; }
id_of()   { awk -v n="$1" '$1==n{print $2}' "$W"; }
# boxes: every box of this run as "<label>\t<discobox-id>\t<pr>" — "#<issue>" delivering, "t#<issue>" triaging.
boxes()   { workers | awk -F'\t' '{print "#"$1"\t"$2"\t"$3}'; grep -v '^\s*$' "$P" | awk -F'\t' '{print "t#"$2"\t"$1"\t-"}'; }
# pool_set <discobox-id> <issue> <state>: one pool row per box.
pool_set() { { awk -F'\t' -v i="$1" '$1!=i' "$P"; printf '%s\t%s\t%s\n' "$1" "$2" "$3"; } >"$P.tmp" && mv "$P.tmp" "$P"; }

cmd_status() {
	local j
	j=$(box ls -o json 2>/dev/null) || { echo "$j"; return 1; }
	boxes | while IFS=$'\t' read -r n id pr; do
		echo "$j" | jq -r --arg id "$id" --arg n "$n" --arg pr "$pr" '(.sandboxes // .)[] | select(.id==$id) |
			(((.runtime.agentStatus.sources // []) | map(select(.slug=="primary" or .slug==null))) + [{}])[0] as $g |
			"\($n) pr=\($pr) \(.id) \(.runtime.runtimeState) \(if (.displayName|startswith("✳")) then "idle" else "busy" end) head=\(($g.headCommit // "?")[0:8]) clean=\($g.clean)"'
	done
}

cmd_screen() { # screen <discobox-id> [lines] [scrollback]
	local u; u=$(use ai.discobox.sandbox '^discobox admin terminal ls'); need "$u" "terminal screen" || return 1
	discobox-access run --use "$u" -- discobox admin terminal screen primary --discobox-id "$1" ${3:+--scrollback "$3"} </dev/null 2>&1 |
		grep -v '^\s*$' | tail -"${2:-25}" | cut -c1-170
}

cmd_say() { # say <discobox-id> <keys or text>...
	local id=$1 u; shift
	u=$(use ai.discobox.sandbox '^discobox admin terminal input'); need "$u" "terminal input" || return 1
	discobox-access run --use "$u" -- discobox admin terminal input primary --discobox-id "$id" "$@" </dev/null 2>&1 | tail -1
}

cmd_power() { # power start|stop|restart <discobox-id>...: the top-level form when a use names it, else the admin one
	local verb=$1 u; shift
	u=$(use ai.discobox.sandbox '^discobox start <')
	if [ -n "$u" ]; then
		discobox-access run --use "$u" -- discobox "$verb" "$@" </dev/null 2>&1 | tail -3
		return
	fi
	u=$(use ai.discobox.sandbox '^discobox admin box start'); need "$u" "start/stop" || return 1
	discobox-access run --use "$u" -- discobox admin box "$verb" "$@" </dev/null 2>&1 | tail -3
}

# running <discobox-id> [seconds]: wait until the discobox is running and its harness is back at a prompt.
running() {
	local id=$1 until=$((SECONDS + ${2:-600})) st
	while [ $SECONDS -lt $until ]; do
		st=$(box get "$id" -o json 2>/dev/null | jq -r '.runtime.runtimeState // empty')
		if [ "$st" = running ] && cmd_screen "$id" 4 | grep -q '❯'; then return 0; fi
		sleep 10
	done
	return 1
}

cmd_approve() { # approve pending com.github.api use requests from workers, once each
	local u ua reqs
	u=$(use ai.discobox.sandbox '^discobox secret request ls'); need "$u" "request ls" || return 1
	ua=$(use ai.discobox.sandbox '^discobox secret request approve'); need "$ua" "approve" || return 1
	reqs=$(discobox-access run --use "$u" -- discobox secret request ls -o json </dev/null 2>/dev/null |
		jq -r '.secretRequests[]? | select(.status=="pending") | "\(.id) \(.sandboxId) \(.purpose) \(.wellKnownId)"')
	while read -r rid sid purpose wk; do
		[ -z "$rid" ] && continue
		grep -qx "$rid" "$ORCH_DIR/tried.txt" && continue
		echo "$rid" >>"$ORCH_DIR/tried.txt"
		local n; n=$(awk -v s="$sid" '$2==s{print $1}' "$W")
		[ -z "$n" ] && n=$(awk -v s="$sid" '$1==s{print "t"$2}' "$P")
		if [ -n "$n" ] && [ "$purpose" = use ] && [ "$wk" = com.github.api ]; then
			local res; res=$(discobox-access run --use "$ua" -- discobox secret request approve "$rid" </dev/null 2>&1 | tail -3 | head -1 | cut -c1-300)
			echo "$(date -u +%FT%H:%M) approve $rid for #$n: $res" >>"$ORCH_DIR/approvals.log"
			case "$res" in GRANT*) echo "approved #$n $rid" ;; *) echo "REFUSED #$n $rid: $res" ;; esac
		else
			echo "NEEDS LEAD: $rid from $sid ($purpose ${wk:-custom})"
		fi
	done <<<"$reqs"
}

cmd_kick() { # type "continue" into a worker whose last screen rows show "Login expired" — once per 15 min each
	local now last
	touch "$ORCH_DIR/kicked.txt"
	boxes | while IFS=$'\t' read -r n id pr; do
		now=$(date +%s)
		last=$(awk -v i="$id" '$1==i{print $2}' "$ORCH_DIR/kicked.txt" | tail -1)
		[ -n "$last" ] && [ $((now - last)) -lt 900 ] && continue
		if cmd_screen "$id" 8 | grep -qi 'login expired'; then
			echo "$id $now" >>"$ORCH_DIR/kicked.txt"
			echo "$(date -u +%FT%H:%M) $n Login expired -> continue: $(cmd_say "$id" continue Enter)" | tee -a "$ORCH_DIR/kicks.log"
		fi
	done
}

cmd_prs() { # one line per worker PR: <pr> <issue> <state> <merged> <mergeable_state>
	need "$(use github 'pulls \(with query parameters\), repos/[^ ]+/pulls/<number>')" "PR reads" || return 1
	workers | while IFS=$'\t' read -r n id pr; do
		[ "$pr" = - ] && continue
		gh_ api "repos/$REPO/pulls/$pr" --jq "\"$pr $n \(.state) \(.merged_at != null) \(.mergeable_state)\"" 2>/dev/null | tail -1 || true
	done
	return 0
}

cmd_rebase() { # rebase <issue>: start the worker if needed and have it rebase its PR branch
	local n=$1 id pr
	id=$(id_of "$n"); pr=$(awk -v n="$n" '$1==n{print $3}' "$W")
	cmd_power start "$id" >/dev/null
	running "$id" || { echo "#$n did not come back to a prompt; rebase not asked"; return 1; }
	local out
	out=$(cmd_say "$id" "PR #$pr now conflicts with main. Fetch main from $REPO, rebase discobox/issue-$n onto it, resolve the conflicts so both changes are kept, run the affected tests, and push to discobox/issue-$n only with git push --force-with-lease=refs/heads/discobox/issue-$n:<its current head on GitHub>. Then keep CI green." Enter)
	echo "$out"
	case "$out" in 20*) echo "$pr $(date +%s)" >>"$ORCH_DIR/rebased.txt" ;; esac
}

cmd_after_merge() { # wait for GitHub to recompute, then rebase each dirty PR (once per 15 min)
	local s now last
	for _ in $(seq 1 10); do s=$(cmd_prs); echo "$s" | grep ' open ' | grep -q ' unknown$' || break; sleep 20; done
	echo "$s"
	now=$(date +%s)
	echo "$s" | awk '$3=="open" && $5=="dirty"{print $1, $2}' | while read -r pr n; do
		last=$(awk -v p="$pr" '$1==p{print $2}' "$ORCH_DIR/rebased.txt" | tail -1)
		if [ -n "$last" ] && [ $((now - last)) -lt 900 ]; then echo "#$pr already asked to rebase"; continue; fi
		echo "#$pr (issue $n): $(cmd_rebase "$n")"
	done
}

cmd_untriaged() { # untriaged: open issues with no triaged or platform/* label and no box on them, oldest first
	local u page out all=
	u=$(use github '^gh api GET repos/[^ ]+/issues \(with query parameters\)'); need "$u" "issue list" || return 1
	for page in $(seq 1 20); do
		out=$(discobox-access run --use "$u" -- gh api "repos/$REPO/issues?state=open&sort=created&direction=asc&per_page=100&page=$page" </dev/null 2>/dev/null) || break
		[ "$(echo "$out" | jq 'length' 2>/dev/null)" -gt 0 ] 2>/dev/null || break
		all+=$(echo "$out" | jq -r '.[] | select(.pull_request | not)
			| select([.labels[].name] | any(. == "triaged" or startswith("platform/")) | not)
			| "\(.number)\t\(.title)"')$'\n'
	done
	printf '%s' "$all" | awk -F'\t' 'NR==FNR{held[$1]=1; next} $1!="" && !held[$1]' <({ cut -f2 "$P"; cut -f1 "$W"; }) -
}

verdict() { cmd_screen "$1" 40 | grep -o "triaged #$2: deliver=.*" | tail -1; }

cmd_verdicts() { # print TRIAGED for each triaging pool box whose screen now ends on its verdict line, and mark it done
	local id n st v
	while IFS=$'\t' read -r id n st; do
		[ "$st" = triaging ] || continue
		v=$(verdict "$id" "$n")
		[ -z "$v" ] && continue
		pool_set "$id" "$n" "done"
		printf '%s\t%s\t%s\n' "$(date -u +%FT%H:%M)" "$id" "$v" >>"$ORCH_DIR/verdicts.log"
		echo "TRIAGED $id $v"
	done < <(grep -v '^\s*$' "$P")
}

cmd_pool() { # one line per pool box: <discobox-id> <issue> <state> <its last verdict>
	local id n st
	while IFS=$'\t' read -r id n st; do
		echo "$id #$n $st $(grep -F "$id"$'\t'"triaged #$n:" "$ORCH_DIR/verdicts.log" 2>/dev/null | tail -1 | cut -f3)"
	done < <(grep -v '^\s*$' "$P")
}

cmd_triage() { # triage <discobox-id> <issue>: clear an idle box's context and hand it the next issue (a merged worker rejoins the pool)
	local id=$1 n=$2
	cmd_power start "$id" >/dev/null
	running "$id" || { echo "$id did not come back to a prompt; #$n not handed over"; return 1; }
	cmd_say "$id" /clear Enter >/dev/null
	sleep 5
	running "$id" 120 || { echo "$id did not come back after /clear; #$n not handed over"; return 1; }
	awk -F'\t' -v i="$id" '$2!=i' "$W" >"$W.tmp" && mv "$W.tmp" "$W"
	pool_set "$id" "$n" triaging
	cmd_say "$id" "Triage issue #$n in discobox-ai/discobox." Enter
}

cmd_deliver() { # deliver <issue>: the pool box that triaged it goes on to deliver it, keeping its context
	local n=$1 id
	id=$(awk -F'\t' -v n="$n" '$2==n{print $1}' "$P" | tail -1)
	[ -n "$id" ] || { echo "no pool box holds #$n"; return 1; }
	cmd_power start "$id" >/dev/null
	running "$id" || { echo "$id did not come back to a prompt; #$n not handed over"; return 1; }
	awk -F'\t' -v i="$id" '$1!=i' "$P" >"$P.tmp" && mv "$P.tmp" "$P"
	printf '%s\t%s\t-\n' "$n" "$id" >>"$W"
	cmd_say "$id" "Deliver issue #$n in discobox-ai/discobox." Enter
}

cmd_watch() { # watch [minutes]: approve, kick, and exit on a PR merge/conflict, a triage verdict, a lead-needed request, low disk, or the heartbeat
	local end=$((SECONDS + ${1:-30} * 60)) tick=0 prev cur out free
	snap() { awk '{m=$5; if (m!="dirty") m="ok"; print $1, $3, $4, m}' | sort; }
	local prs
	prs=$(cmd_prs) || { echo "$prs"; exit 0; }
	prev=$(echo "$prs" | snap)
	while :; do
		out=$(cmd_approve)
		echo "$out" | grep -E '^(REFUSED|NEEDS LEAD|MISSING USE)' && exit 0
		prs=$(cmd_prs) || { echo "$prs"; exit 0; }
		tick=$((tick + 1))
		[ $((tick % 4)) -eq 1 ] && cmd_kick >/dev/null
		out=$(cmd_verdicts)
		[ -n "$out" ] && { echo "$out"; exit 0; }
		free=$(df -BG --output=avail / | tail -1 | tr -dc 0-9)
		[ "$free" -lt "${ORCH_MIN_DISK_G:-60}" ] && { echo "DISK LOW: ${free}G free"; exit 0; }
		cur=$(echo "$prs" | snap)
		if [ -n "$cur" ] && [ "$cur" != "$prev" ]; then echo "PR CHANGE:"; diff <(echo "$prev") <(echo "$cur") | grep '^[<>]'; exit 0; fi
		[ $SECONDS -ge $end ] && { echo "HEARTBEAT ${free}G free"; cmd_status; exit 0; }
		sleep 60
	done
}

case "${1:-}" in
status | screen | say | power | approve | kick | prs | rebase | watch | untriaged | pool | triage | deliver) c=$1; shift; "cmd_$c" "$@" ;;
after-merge) shift; cmd_after_merge "$@" ;;
*) sed -n '2,14p' "$0"; echo "usage: $0 status|screen|say|power|approve|kick|prs|rebase|after-merge|watch|untriaged|pool|triage|deliver ..."; exit 2 ;;
esac
