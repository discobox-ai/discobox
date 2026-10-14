#!/usr/bin/env bash
# orchestrate.sh — the lead discobox's loop over the worker discoboxes it created.
#
# State lives in $ORCH_DIR (default ~/.local/state/orchestrate), which survives a
# wiped scratchpad: workers.tsv ("<issue>\t<discobox-id>\t<pr>", pr may be "-"),
# triage.tsv (triage mode: "<discobox-id>\t<issue>\t<triaging|done|retired>"), verdicts.log,
# tried.txt (request IDs already answered), rebased.txt, retired.txt, idle-seen.txt,
# idle-reported.txt, github.last, watch-issues.txt, closed-issues.txt, approvals.log, kicks.log.
# A box belongs to one issue for its whole life: it is in triage.tsv while it triages that issue
# and in workers.tsv once it delivers it, and it is never handed another. ORCH_MAX (default 4)
# caps how many of this run's boxes are up at once, triaging and delivering together.
#
# Use IDs are looked up by their descriptions on every call, so a renewed grant
# is picked up without editing anything. discobox-access always gets
# </dev/null: it shows the judge what a command reads on stdin, and in a loop it
# would read the loop's input.
set -uo pipefail
# nix develop exports $out, and a local of that name inherits the export: a
# page of issue JSON in it would reach every child's environment and fail exec.
export -n out

ORCH_DIR=${ORCH_DIR:-$HOME/.local/state/orchestrate}
REPO=${ORCH_REPO:-discobox-ai/discobox}
MAX=${ORCH_MAX:-4}
mkdir -p "$ORCH_DIR"
W=$ORCH_DIR/workers.tsv
T=$ORCH_DIR/triage.tsv
# A run started before triage.tsv had its name kept the same rows in pool.tsv.
[ -f "$ORCH_DIR/pool.tsv" ] && [ ! -f "$T" ] && mv "$ORCH_DIR/pool.tsv" "$T"
touch "$W" "$T" "$ORCH_DIR/tried.txt" "$ORCH_DIR/rebased.txt" "$ORCH_DIR/retired.txt"

# A box's harness says whether it is working in the title of its primary terminal (Claude Code leads
# it with "✳" when idle). That is not .displayName: a box that has been given a name is listed by the
# name instead (ADR 26-10-12-026), so the title is read from where the server reads it, the newest
# primary session that has one.
JQ_TITLE='def title: ([(.runtime.agentStatus.sessions // [])[] | select(.primary and ((.title // "") != ""))]
	| sort_by(.startedAt // "") | last | .title) // "";'

# use <credential-name> <regex>: the live use ID whose description matches.
# The listing fails now and then for a moment, so an empty answer is asked again before it counts.
use() {
	local id i
	for i in 1 2 3; do
		id=$(discobox-access list --json </dev/null 2>/dev/null |
			jq -r --arg c "$1" --arg re "$2" \
				'[.credentials[] | select(.name==$c) | .uses[] | select(.description|test($re))] | last | .useId // empty')
		[ -n "$id" ] && { echo "$id"; return; }
		[ "$i" -lt 3 ] && sleep 5
	done
}
# need <use-id> <what>: a missing use is printed as MISSING USE (watch exits on it) and fails the caller.
need() { [ -n "$1" ] && return 0; echo "MISSING USE: no live use for $2; ask for it (SKILL.md §1)"; return 1; }
box()  { local u; u=$(use ai.discobox.sandbox '^discobox admin box ls'); need "$u" "box ls/get" || return 1; discobox-access run --use "$u" -- discobox admin box "$@" </dev/null; }
gh_()  { local u; u=$(use github 'pulls \(with query parameters\), repos/[^ ]+/pulls/<number>'); need "$u" "PR reads" || return 1; discobox-access run --use "$u" -- gh "$@" </dev/null; }

workers() { grep -v '^\s*$' "$W" || true; }
id_of()   { awk -v n="$1" '$1==n{print $2}' "$W"; }
# boxes: every box of this run as "<label>\t<discobox-id>\t<pr>" — "#<issue>" delivering, "t#<issue>" triaging
# (a retired triage box is left out: it is stopped for good and nothing watches it).
boxes()   { workers | awk -F'\t' '{print "#"$1"\t"$2"\t"$3}'; awk -F'\t' '$1!="" && $3!="retired"{print "t#"$2"\t"$1"\t-"}' "$T"; }
# triage_set <discobox-id> <issue> <state>: one triage row per box.
triage_set() { { awk -F'\t' -v i="$1" '$1!=i' "$T"; printf '%s\t%s\t%s\n' "$1" "$2" "$3"; } >"$T.tmp" && mv "$T.tmp" "$T"; }
# up_count: how many of this run's boxes are not stopped, triaging and delivering alike. This is
# what ORCH_MAX caps: every box that is up builds its own images on the one host disk.
up_count() {
	local j
	j=$(box ls -o json 2>/dev/null) || { echo "$j"; return 1; }
	echo "$j" | jq -r '(.sandboxes // .)[] | select(.runtime.runtimeState != "stopped") | .id' | grep -cFxf <(boxes | cut -f2) || true
}

cmd_slots() { # slots: how many more boxes may be started under ORCH_MAX
	local up
	up=$(up_count) || { echo "$up"; return 1; }
	echo "$((MAX - up)) free of $MAX ($up up)"
}

cmd_status() {
	local j
	j=$(box ls -o json 2>/dev/null) || { echo "$j"; return 1; }
	boxes | while IFS=$'\t' read -r n id pr; do
		echo "$j" | jq -r --arg id "$id" --arg n "$n" --arg pr "$pr" "$JQ_TITLE"'(.sandboxes // .)[] | select(.id==$id) |
			(((.runtime.agentStatus.sources // []) | map(select(.slug=="primary" or .slug==null))) + [{}])[0] as $g |
			"\($n) pr=\($pr) \(.id) \(.runtime.runtimeState) \(if (title|startswith("✳")) then "idle" else "busy" end) head=\(($g.headCommit // "?")[0:8]) clean=\($g.clean)\((.meta.tags // {}) | if has("ready") then " #ready" elif has("blocked") then " #blocked" elif has("merged") then " #merged" else "" end)"'
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

# retire <issue>|<discobox-id> [merged]: mark a finished box for the human to delete, then stop it.
# "discobox tag" writes the tag through the server, which the sandbox role allows only on a box this
# lead created (ADR 26-10-08-447). The lead cannot delete a box. A stopped worker cannot move its own
# state tag (deliver-issue §6), so retire drops ready/blocked and, for a merged PR, sets merged.
cmd_retire() { # retire <issue>|<discobox-id> [merged]: a triage box has no issue in workers.tsv, so take its ID
	local n=$1 id u out tags=(to-delete)
	[ "${2:-}" = merged ] && tags+=(merged)
	case "$n" in sbx_*) id=$n ;; *) id=$(id_of "$n") ;; esac
	[ -n "$id" ] || { echo "#$n: no worker"; return 1; }
	box get "$id" -o json >/dev/null 2>&1 || { echo "$id" >>"$ORCH_DIR/retired.txt"; echo "#$n $id is already deleted"; return 0; }
	u=$(use ai.discobox.sandbox '^discobox tag <'); need "$u" "discobox tag" || return 1
	out=$(discobox-access run --use "$u" -- discobox tag "$id" "${tags[@]}" --rm ready --rm blocked </dev/null 2>&1) ||
		{ echo "#$n $id not tagged: $(echo "$out" | tail -1)"; return 1; }
	echo "$id" >>"$ORCH_DIR/retired.txt"
	awk -F'\t' -v OFS='\t' -v i="$id" '$1==i{$3="retired"} 1' "$T" >"$T.tmp" && mv "$T.tmp" "$T"
	cmd_power stop "$id" >/dev/null
	echo "#$n $id tagged ${tags[*]} and stopped"
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
		[ -z "$n" ] && n=$(awk -v s="$sid" '$1==s{print "t"$2}' "$T")
		if [ -n "$n" ] && [ "$purpose" = use ] && [ "$wk" = com.github.api ]; then
			local res; res=$(discobox-access run --use "$ua" -- discobox secret request approve "$rid" </dev/null 2>&1 | tail -3 | head -1 | cut -c1-300)
			echo "$(date -u +%FT%H:%M) approve $rid for #$n: $res" >>"$ORCH_DIR/approvals.log"
			case "$res" in GRANT*) echo "approved #$n $rid" ;; *) echo "REFUSED #$n $rid: $res" ;; esac
		else
			echo "NEEDS LEAD: $rid from $sid ($purpose ${wk:-custom})"
		fi
	done <<<"$reqs"
}

# firstrun <discobox-id>: answer Claude Code's one-time "Make auto mode your default permission
# mode?" dialog with "No, keep bypass permissions" (the user's choice, 2026-10-08). It appears when
# a worker restarts, and it swallows whatever is typed next.
firstrun() {
	cmd_screen "$1" 14 | grep -q 'Make auto mode your default' || return 1
	cmd_say "$1" Down Enter >/dev/null
	sleep 3
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

cmd_after_merge() { # wait for GitHub to recompute, retire merged PRs' workers, then rebase each dirty PR (once per 15 min)
	local s now last
	for _ in $(seq 1 10); do s=$(cmd_prs); echo "$s" | grep ' open ' | grep -q ' unknown$' || break; sleep 20; done
	echo "$s"
	# A merged PR's worker is done: tag it for the human to delete and stop it, once. Its slot is
	# free for a new box; the box itself is never given another issue.
	echo "$s" | awk '$3=="closed" && $4=="true"{print $2}' | while read -r n; do
		grep -qx "$(id_of "$n")" "$ORCH_DIR/retired.txt" && continue
		cmd_retire "$n" merged
	done
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
	printf '%s' "$all" | awk -F'\t' 'FILENAME==ARGV[1]{held[$1]=1; next} $1!="" && !held[$1]' <({ cut -f2 "$T"; cut -f1 "$W"; }) -
}

verdict() { cmd_screen "$1" 40 | grep -o "triaged #$2: deliver=.*" | tail -1; }

cmd_verdicts() { # print TRIAGED for each triaging box whose screen now ends on its verdict line, and mark it done
	local id n st v
	while IFS=$'\t' read -r id n st; do
		[ "$st" = triaging ] || continue
		v=$(verdict "$id" "$n")
		[ -z "$v" ] && continue
		triage_set "$id" "$n" "done"
		printf '%s\t%s\t%s\n' "$(date -u +%FT%H:%M)" "$id" "$v" >>"$ORCH_DIR/verdicts.log"
		echo "TRIAGED $id $v"
	done < <(grep -v '^\s*$' "$T")
}

cmd_triages() { # one line per triage box: <discobox-id> <issue> <state> <its verdict>
	local id n st
	while IFS=$'\t' read -r id n st; do
		echo "$id #$n $st $(grep -F "$id"$'\t'"triaged #$n:" "$ORCH_DIR/verdicts.log" 2>/dev/null | tail -1 | cut -f3)"
	done < <(grep -v '^\s*$' "$T")
}

cmd_triage() { # triage <issue>: launch a new box for this one issue, when a slot is free under ORCH_MAX
	local n=$1 u up out id
	if awk -F'\t' -v n="$n" '$2==n{f=1} END{exit !f}' "$T" || [ -n "$(id_of "$n")" ]; then echo "#$n already has a box"; return 1; fi
	up=$(up_count) || { echo "$up"; return 1; }
	[ "$up" -lt "$MAX" ] || { echo "no free slot: $up of $MAX boxes are up; #$n not launched"; return 1; }
	u=$(use ai.discobox.sandbox '^discobox new -d'); need "$u" "discobox new" || return 1
	out=$(discobox-access run --use "$u" -- discobox new -d -C "https://github.com/$REPO@main" -p "Triage issue #$n in $REPO." </dev/null 2>&1)
	id=$(echo "$out" | grep -o 'sbx_[a-z0-9]*' | head -1)
	[ -n "$id" ] || { echo "#$n not launched: $(echo "$out" | tail -1)"; return 1; }
	triage_set "$id" "$n" triaging
	echo "#$n $id launched"
}

cmd_deliver() { # deliver <issue>: the box that triaged it goes on to deliver it, keeping its context
	local n=$1 id st up
	id=$(awk -F'\t' -v n="$n" '$2==n && $3!="retired"{print $1}' "$T" | tail -1)
	[ -n "$id" ] || { echo "no triage box holds #$n"; return 1; }
	# A box stopped while its verdict waited on the user takes a slot again when it starts.
	st=$(box get "$id" -o json 2>/dev/null | jq -r '.runtime.runtimeState // empty')
	if [ "$st" = stopped ]; then
		up=$(up_count) || { echo "$up"; return 1; }
		[ "$up" -lt "$MAX" ] || { echo "no free slot: $up of $MAX boxes are up; #$n not handed over"; return 1; }
	fi
	cmd_power start "$id" >/dev/null
	running "$id" || { echo "$id did not come back to a prompt; #$n not handed over"; return 1; }
	awk -F'\t' -v i="$id" '$1!=i' "$T" >"$T.tmp" && mv "$T.tmp" "$T"
	printf '%s\t%s\t-\n' "$n" "$id" >>"$W"
	cmd_say "$id" "Deliver issue #$n in $REPO." Enter
}

# idle: print "IDLE <label> <discobox-id>" for each box (labelled as boxes() does) whose harness has been idle (Claude Code titles
# it "✳ …") on two calls in a row and has not been reported since it was last busy. idle-seen.txt holds
# the previous call's idle IDs; idle-reported.txt the reported ones, so a restarted watch stays quiet
# about a worker the lead already looked at until it works again. A reported worker seen working
# again prints "RESUMED <label> <discobox-id>": someone answered it, and the lead's status is stale.
cmd_idle() {
	local j now
	touch "$ORCH_DIR/idle-seen.txt" "$ORCH_DIR/idle-reported.txt"
	j=$(box ls -o json 2>/dev/null) || return 0
	now=$(echo "$j" | jq -r "$JQ_TITLE"'(.sandboxes // .)[] | select(.runtime.runtimeState=="running" and (title|startswith("✳"))) | .id')
	# A worker seen busy (or stopped) clears its report, so its next idle stretch is reported again.
	local busy
	busy=$(echo "$j" | jq -r "$JQ_TITLE"'(.sandboxes // .)[] | select(.runtime.runtimeState=="running" and (title|startswith("✳")|not)) | .id')
	# Only a worker still running and now working has resumed; one that stopped has not.
	grep -vFxf <(echo "$now") "$ORCH_DIR/idle-reported.txt" | grep -Fxf <(echo "$busy") | while read -r id; do
		echo "RESUMED $(boxes | awk -F'\t' -v i="$id" '$2==i{print $1}') $id"
	done
	grep -Fxf <(echo "$now") "$ORCH_DIR/idle-reported.txt" >"$ORCH_DIR/idle-reported.tmp" || true
	mv "$ORCH_DIR/idle-reported.tmp" "$ORCH_DIR/idle-reported.txt"
	boxes | while IFS=$'\t' read -r n id pr; do
		echo "$now" | grep -qx "$id" || continue
		grep -qx "$id" "$ORCH_DIR/idle-seen.txt" || continue
		grep -qx "$id" "$ORCH_DIR/idle-reported.txt" && continue
		# A screen still saying "esc to interrupt" is working (the title lags). Idle while a background
		# shell or monitor runs is waiting on its own work (a build, CI), not
		# on us, unless a question dialog is open.
		local scr; scr=$(cmd_screen "$id" 8)
		# "Login expired" is not a real expiry: kick answers it, and the lead never needs to see it.
		if echo "$scr" | grep -qi 'login expired'; then
			cmd_kick >/dev/null
			continue
		fi
		firstrun "$id" && continue
		if ! echo "$scr" | grep -q 'Enter to select' &&
			echo "$scr" | grep -qE 'esc to interrupt|still running|· [0-9]+ (shells?|monitors?)'; then
			continue
		fi
		echo "$id" >>"$ORCH_DIR/idle-reported.txt"
		echo "IDLE $n $id"
	done
	echo "$now" >"$ORCH_DIR/idle-seen.txt"
}

# github: one line per issue and PR the lead cares about, "issue <n> open|closed" and
# "pr <n> issue-<n> open|merged|closed", and "ci main <sha> <result>" (latest finished run): every worker's issue and PR, and any issue listed in
# watch-issues.txt (ones the lead filed or was told about). A worker PR GitHub lists that
# workers.tsv does not know yet is recorded there. Closed issues are cached in
# closed-issues.txt, so a pass costs one PR list call and one call per open issue; "github prs"
# makes only the PR list call, since every call is judged and takes seconds.
cmd_github() {
	local u n st prs
	u=$(use github '^gh api GET repos/[^ ]+/issues/<number> and'); need "$u" "issue reads" || return 1
	touch "$ORCH_DIR/watch-issues.txt" "$ORCH_DIR/closed-issues.txt"
	prs=$(gh_ api "repos/$REPO/pulls?state=all&sort=updated&direction=desc&per_page=50" \
		--jq '.[] | select(.head.ref|startswith("discobox/issue-")) |
			"\(.number) \(.head.ref|sub("discobox/";"")) \(if .merged_at then "merged" else .state end)"' 2>/dev/null) || return 1
	workers | while IFS=$'\t' read -r n id pr; do
		[ "$pr" = - ] || continue
		pr=$(echo "$prs" | awk -v b="issue-$n" '$2==b{print $1; exit}')
		[ -n "$pr" ] && sed -i "s/^$n\t$id\t-\$/$n\t$id\t$pr/" "$W"
	done
	echo "$prs" | grep -Fwf <(workers | awk -F'\t' '{print "issue-"$1}') | sed 's/^/pr /'
	[ "${1:-}" = prs ] && return 0
	# main's latest finished CI run, "ci main <sha> success|failure|...", so a red main is seen. A run
	# still going is left out, and so is one cancelled by a newer push: on a busy main either would
	# wake the lead for nothing.
	local uc
	uc=$(use github 'actions/runs \(with query'); need "$uc" "CI reads" || return 1
	discobox-access run --use "$uc" -- gh api "repos/$REPO/actions/runs?branch=main&status=completed&per_page=10" \
		--jq '[.workflow_runs[] | select(.conclusion != "cancelled")][0] | "ci main \(.head_sha[0:8]) \(.conclusion)"' </dev/null 2>/dev/null | tail -1
	{ workers | cut -f1; cat "$ORCH_DIR/watch-issues.txt"; } | grep -E '^[0-9]+$' | sort -un | while read -r n; do
		if grep -qx "$n" "$ORCH_DIR/closed-issues.txt"; then echo "issue $n closed"; continue; fi
		st=$(discobox-access run --use "$u" -- gh api "repos/$REPO/issues/$n" --jq .state </dev/null 2>/dev/null | tail -1)
		case "$st" in
		closed) echo "$n" >>"$ORCH_DIR/closed-issues.txt"; echo "issue $n closed" ;;
		open) echo "issue $n open" ;;
		*) echo "issue $n ?" ;;
		esac
	done
}

cmd_watch() { # watch [minutes]: approve, kick, and exit on an issue or PR change on GitHub, a PR conflict, a triage verdict, a lead-needed request, a box going idle or resuming, low disk, or the heartbeat
	local end=$((SECONDS + ${1:-30} * 60)) tick=0 prev cur out free idle
	snap() { awk '{m=$5; if (m!="dirty") m="ok"; print $1, $3, $4, m}' | sort; }
	local prs gh0 gh1 pr0 pr1 i
	# The lead needs no dev loop, but a session restart brings task dev's image watcher back, and
	# it builds into a cache of tens of GB on the pool's one disk. Stop it here, every watch.
	pkill -f '[d]iscobox-docker-image-watch' 2>/dev/null || true
	prs=$(cmd_prs) || { echo "$prs"; exit 0; }
	prev=$(echo "$prs" | snap)
	# The baseline is the last state a watch reported, kept in github.last, so a merge or close
	# that happened between two watches (or while the lead was busy) is still reported.
	if [ -s "$ORCH_DIR/github.last" ]; then gh0=$(cat "$ORCH_DIR/github.last"); else gh0=$(cmd_github | sort); echo "$gh0" >"$ORCH_DIR/github.last"; fi
	pr0=$(echo "$gh0" | grep '^pr ')
	while :; do
		out=$(cmd_approve)
		echo "$out" | grep -E '^(REFUSED|NEEDS LEAD|MISSING USE)' && exit 0
		prs=$(cmd_prs) || { echo "$prs"; exit 0; }
		tick=$((tick + 1))
		[ $((tick % 4)) -eq 1 ] && cmd_kick >/dev/null
		out=$(cmd_verdicts)
		[ -n "$out" ] && { echo "$out"; exit 0; }
		idle=$(cmd_idle)
		if [ -n "$idle" ]; then
			echo "$idle" | while read -r what n id; do
				echo "$what $n $id"
				[ "$what" = IDLE ] && cmd_screen "$id" 15 | sed 's/^/    /'
			done
			exit 0
		fi
		free=$(df -BG --output=avail / | tail -1 | tr -dc 0-9)
		[ "$free" -lt "${ORCH_MIN_DISK_G:-60}" ] && { echo "DISK LOW: ${free}G free"; exit 0; }
		cur=$(echo "$prs" | snap)
		if [ -n "$cur" ] && [ "$cur" != "$prev" ]; then echo "PR CHANGE:"; diff <(echo "$prev") <(echo "$cur") | grep '^[<>]'; exit 0; fi
		[ $SECONDS -ge $end ] && { echo "HEARTBEAT ${free}G free"; cmd_status; exit 0; }
		# Issues once a pass, PRs every 15s between passes: an issue closed or reopened, a PR opened,
		# merged or closed. The human waits on what follows. A "?" or a use the listing briefly lost is a
		# failed read, not a change.
		gh1=$(cmd_github | sort)
		if [ -n "$gh1" ] && ! echo "$gh1" | grep -qE ' \?$|MISSING USE' && [ "$gh1" != "$gh0" ]; then
			echo "GITHUB:"; comm -13 <(echo "$gh0") <(echo "$gh1") | sed 's/^/  now /'
			echo "$gh1" >"$ORCH_DIR/github.last"
			exit 0
		fi
		for i in 1 2 3 4; do
			sleep 15
			pr1=$(cmd_github prs | sort) || continue
			echo "$pr1" | grep -q 'MISSING USE' && continue
			if [ -n "$pr1" ] && [ "$pr1" != "$pr0" ]; then
				echo "GITHUB:"; comm -13 <(echo "$pr0") <(echo "$pr1") | sed 's/^/  now /'
				{ echo "$gh0" | grep -v '^pr '; echo "$pr1"; } | sort >"$ORCH_DIR/github.last"
				exit 0
			fi
		done
	done
}

case "${1:-}" in
status | screen | say | power | retire | approve | kick | idle | github | prs | rebase | watch | slots | untriaged | triages | triage | deliver) c=$1; shift; "cmd_$c" "$@" ;;
after-merge) shift; cmd_after_merge "$@" ;;
*) sed -n '2,16p' "$0"; echo "usage: $0 status|screen|say|power|retire|approve|kick|idle|github|prs|rebase|after-merge|watch|slots|untriaged|triages|triage|deliver ..."; exit 2 ;;
esac
