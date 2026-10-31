#!/usr/bin/env bash
#---
# name: GitHub repository
# description: New activity on a repository's issues, pull requests and Actions runs
# interval: 60s
# options:
# - {name: repo, required: true, description: owner/name}
# - {name: watch, description: "issues, prs, actions, comma-separated; all three when omitted and no ids are given"}
# - {name: issue, description: "issue numbers to watch, comma-separated"}
# - {name: pr, description: "pull request numbers to watch, comma-separated"}
# - {name: run, description: "Actions run ids to watch, comma-separated"}
#---
# The image's github-repo monitor (ADR 26-10-31-073 §9). It reports what
# happens after it is enabled, never what was already there: the first run
# records where things stand and prints nothing (§3), except that an instance
# watching only ids whose every item is already closed or completed says so,
# and ends.
#
# It reads the REST API through gh, with the box's credential. Each run reads
# what changed since the last one began:
#   issues  opened, closed, reopened; comments
#   prs     opened, closed, merged, reopened, ready for review; comments;
#           reviews, with their inline comments; for a PR named by id, its
#           head's checks reaching a conclusion
#   actions runs completing, with a failed run's failed jobs and steps
# It does not read the repository events feed, which carries no Actions runs
# or checks and lags by minutes to hours.
set -euo pipefail

repo="${DISCOBOX_OPT_REPO:?}"
state="${DISCOBOX_MONITOR_STATE:?}"
if [[ ! "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "repo must be owner/name, not '$repo'" >&2
  exit 2
fi

# list turns "1, 2,3" into "1 2 3", refusing anything but numbers.
list() {
  local numbers=() item parts
  IFS=', ' read -r -a parts <<<"$1"
  for item in "${parts[@]}"; do
    [[ -z "$item" ]] && continue
    if [[ ! "$item" =~ ^[0-9]+$ ]]; then
      echo "'$item' is not a number" >&2
      exit 2
    fi
    numbers+=("$item")
  done
  echo "${numbers[*]:-}"
}
issues=$(list "${DISCOBOX_OPT_ISSUE:-}")
prs=$(list "${DISCOBOX_OPT_PR:-}")
runs=$(list "${DISCOBOX_OPT_RUN:-}")

# Which kinds are watched: as asked, else those ids were named for, else all.
watch="${DISCOBOX_OPT_WATCH:-}"
if [[ -z "$watch" ]]; then
  [[ -n "$issues" ]] && watch+="issues,"
  [[ -n "$prs" ]] && watch+="prs,"
  [[ -n "$runs" ]] && watch+="actions,"
  watch="${watch:-issues,prs,actions}"
fi
want_issues=false want_prs=false want_actions=false
IFS=', ' read -r -a kinds <<<"$watch"
for kind in "${kinds[@]}"; do
  case "$kind" in
    issues) want_issues=true ;;
    prs) want_prs=true ;;
    actions) want_actions=true ;;
    "") ;;
    *) echo "watch takes issues, prs and actions, not '$kind'" >&2; exit 2 ;;
  esac
done
# Only ids, and nothing watched whole: such an instance can finish.
only_ids=true
{ $want_issues && [[ -z "$issues" ]]; } && only_ids=false
{ $want_prs && [[ -z "$prs" ]]; } && only_ids=false
{ $want_actions && [[ -z "$runs" ]]; } && only_ids=false

# Without a credential gh says to log in, which is not how a discobox gets one:
# say what is, since this line is what the monitor.failed event carries.
if [[ -z "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ]] && ! gh auth status >/dev/null 2>&1; then
  echo "no GitHub credential in this box: ask for one with discobox-access (GH_TOKEN, for api.github.com)" >&2
  exit 4
fi

api() { gh api -H 'Accept: application/vnd.github+json' "$@"; }
# Each run reads what happened in (since, now], in GitHub's own clock — the
# Date of a request that costs no rate limit — so consecutive runs tile time
# with nothing read twice or missed, whatever this box's clock says.
now=$({ api -i rate_limit 2>/dev/null || true; } | awk -F': ' 'tolower($1) == "date" && !seen { print $2; seen = 1 }' | tr -d '\r')
now=$(date -u -d "${now:-now}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)
first=false
if [[ -f "$state/since" ]]; then
  since=$(<"$state/since")
else
  first=true
  since="$now"
fi
items='{}' tracked='{}'
[[ -f "$state/items.json" ]] && items=$(<"$state/items.json")
[[ -f "$state/runs.json" ]] && tracked=$(<"$state/runs.json")

# What this run reports is held back until the whole of it has succeeded, and
# only then printed and the state saved: a run that fails part way — a rate
# limit, a 5xx — queues nothing, and the next reads the same window again
# rather than queuing half of it twice.
buffer=$(mktemp)
trap 'rm -f "$buffer" "$buffer.inline"' EXIT
exec 3>&1 >"$buffer"

# issue_events prints the repository's issue events, one per line, newest
# first, back to the first page that reaches the start of the window.
issue_events() {
  # Not `out`: a name a shell may already export (nix's does), which a local
  # keeps exporting — and a page of events in the environment is past what
  # exec allows.
  local page=1 events_page
  while :; do
    events_page=$(api "repos/$repo/issues/events?per_page=100&page=$page")
    jq -c '.[]' <<<"$events_page"
    (($(jq length <<<"$events_page") == 100)) || break
    [[ $(jq -r '.[-1].created_at' <<<"$events_page") > "$since" ]] || break
    page=$((page + 1))
  done
}

# wanted reports whether an issue or PR number is one this instance watches.
wanted() { # kind number
  local ids
  case "$1" in
    issue) $want_issues || return 1; ids="$issues" ;;
    pr) $want_prs || return 1; ids="$prs" ;;
  esac
  [[ -z "$ids" ]] && return 0
  [[ " $ids " == *" $2 "* ]]
}

# The numbers this instance could report on, as a JSON filter's input.
filter_json=$(jq -nc --arg i "$issues" --arg p "$prs" \
  --argjson wi "$want_issues" --argjson wp "$want_prs" \
  '{issues: ($i | split(" ") | map(select(. != "") | tonumber)),
    prs: ($p | split(" ") | map(select(. != "") | tonumber)),
    wantIssues: $wi, wantPrs: $wp}')
# select_item keeps an object whose .n and .pr say it is watched. It is jq,
# spliced into each filter below, so its $ are jq's.
# shellcheck disable=SC2016
select_item='select(if .pr then ($f.wantPrs and (($f.prs | length) == 0 or (.n as $n | $f.prs | index($n)))) else ($f.wantIssues and (($f.issues | length) == 0 or (.n as $n | $f.issues | index($n)))) end)'

if ! $first; then
  # Opened: issues and PRs created since the last run.
  if $want_issues || $want_prs; then
    api --paginate "repos/$repo/issues?state=all&sort=created&direction=desc&since=$since&per_page=100" \
      | jq -c --arg since "$since" --arg now "$now" --argjson f "$filter_json" '
        .[] | select(.created_at > $since and .created_at <= $now)
        | {n: .number, pr: (.pull_request != null), title, user: .user.login, url: .html_url, body: (.body // "")}
        | '"$select_item"'
        | {kind: (if .pr then "pr.opened" else "issue.opened" end),
           title: "#\(.n) opened by \(.user): \(.title)", url, body}' \
      | tac

    # State changes, from the issue events of the whole repository, newest
    # first, page after page until one reaches back past the window: a run
    # after the box slept covers hours. A merge is also a close, which says
    # nothing more and would replace the merge under their shared key, so a
    # merged PR's close is left out.
    issue_events \
      | jq -sc --arg since "$since" --arg now "$now" --argjson f "$filter_json" '
        [.[] | select(.created_at > $since and .created_at <= $now)
         | select(.event | IN("closed", "reopened", "merged", "ready_for_review"))
         | {n: .issue.number, pr: (.issue.pull_request != null), event, actor: (.actor.login // "someone"), title: .issue.title, url: .issue.html_url}
         | '"$select_item"']
        | ([.[] | select(.event == "merged") | .n]) as $merged
        | reverse[]
        | select(.event != "closed" or (.n as $n | $merged | index($n) | not))
        | {kind: ((if .pr then "pr." else "issue." end) + .event),
           key: ((if .pr then "pr-" else "issue-" end) + "\(.n)-state"),
           title: "#\(.n) \(.event | gsub("_"; " ")) by \(.actor): \(.title)", url}'

    # Comments on issues and PRs' conversations created since the last run.
    api --paginate "repos/$repo/issues/comments?since=$since&per_page=100" \
      | jq -c --arg since "$since" --arg now "$now" --argjson f "$filter_json" '
        .[] | select(.created_at > $since and .created_at <= $now)
        | {n: (.issue_url | split("/") | last | tonumber), pr: (.html_url | test("/pull/")), user: .user.login, url: .html_url, body: (.body // "")}
        | '"$select_item"'
        | {kind: (if .pr then "pr.comment" else "issue.comment" end),
           title: "\(.user) commented on #\(.n): \(.body | split("\n") | map(select(test("\\S"))) | first // "" | .[0:120])",
           url, body}'
  fi

  # Reviews, on each PR updated since the last run.
  if $want_prs; then
    updated=$(api --paginate "repos/$repo/issues?state=all&sort=updated&direction=desc&since=$since&per_page=100" \
      | jq -r '.[] | select(.pull_request != null) | .number')
    for n in $updated; do
      wanted pr "$n" || continue
      api "repos/$repo/pulls/$n/reviews?per_page=100" \
        | jq -c --arg since "$since" --arg now "$now" '.[] | select((.submitted_at // "") > $since and .submitted_at <= $now)' \
        | while read -r review; do
            id=$(jq -r .id <<<"$review")
            # Through a file, not an argument: review comments can be long.
            api "repos/$repo/pulls/$n/reviews/$id/comments?per_page=100" \
              | jq -r '.[] | "\(.path):\(.line // .original_line // "-"): \(.body)\n"' >"$buffer.inline"
            jq -c --arg n "$n" --rawfile inline "$buffer.inline" '
              {kind: "pr.review",
               title: "\(.user.login) \({APPROVED: "approved", CHANGES_REQUESTED: "requested changes on", COMMENTED: "reviewed", DISMISSED: "dismissed a review on"}[.state] // "reviewed") #\($n)",
               summary: (.body // "" | split("\n") | map(select(test("\\S"))) | first // "" | .[0:160]),
               url: .html_url,
               body: ([(.body // ""), $inline] | map(select(. != "")) | join("\n\n"))}' <<<"$review"
          done
    done
  fi
fi

# Named issues and PRs: their state now, and for a PR its head's checks.
done_items=0 named_items=0
for n in $issues; do
  $want_issues || break
  named_items=$((named_items + 1))
  st=$(api "repos/$repo/issues/$n" --jq .state)
  [[ "$st" == closed ]] && done_items=$((done_items + 1))
done
for n in $prs; do
  $want_prs || break
  named_items=$((named_items + 1))
  # Cut to what is used: it is passed to jq as an argument, which has a size
  # limit a PR's body can pass.
  pull=$(api "repos/$repo/pulls/$n" --jq '{state, title, html_url, head: {sha: .head.sha}}')
  sha=$(jq -r .head.sha <<<"$pull")
  [[ $(jq -r .state <<<"$pull") == closed ]] && done_items=$((done_items + 1))
  # The head's checks, reduced to one conclusion: pending while any runs,
  # else failure if any failed, else success.
  checks=$(api "repos/$repo/commits/$sha/check-runs?per_page=100" \
    | jq -c '[.check_runs[] | {name, status, conclusion}]')
  conclusion=$(jq -r '
    if length == 0 then "none"
    elif any(.[]; .status != "completed") then "pending"
    elif any(.[]; .conclusion | IN("failure", "timed_out", "cancelled", "action_required", "startup_failure")) then "failure"
    else "success" end' <<<"$checks")
  previous=$(jq -r --arg n "$n" '.["pr-" + $n].checks // ""' <<<"$items")
  if ! $first && [[ "$conclusion" != "$previous" && "$conclusion" =~ ^(success|failure)$ ]]; then
    jq -c --arg n "$n" --arg c "$conclusion" --arg sha "${sha:0:7}" --argjson pull "$pull" '
      ([.[] | select(.conclusion | IN("failure", "timed_out", "cancelled", "action_required", "startup_failure")) | .name]) as $failed
      | {kind: "pr.checks", key: "pr-\($n)-checks",
         title: (if $c == "failure" then "#\($n) checks failed on \($sha): \($failed | join(", "))" else "#\($n) checks passed on \($sha)" end),
         summary: $pull.title, url: "\($pull.html_url)/checks",
         body: (map("\(.name): \(.conclusion // .status)") | join("\n"))}' <<<"$checks"
  fi
  items=$(jq -c --arg n "$n" --arg c "$conclusion" '.["pr-" + $n].checks = $c' <<<"$items")
done

# Actions runs: every run created since the last run, all pages; the newest
# page, for runs re-run since; and each tracked run still running, fetched by
# id when neither listing has it. A reported run stays in runs.json as
# "done:<updated_at>", so a run seen complete just after `now` is not reported
# again next time; one re-run is updated, and reported again.
if $want_actions; then
  if [[ -n "$runs" ]]; then
    listed='[]'
    fetch="$runs"
  else
    listed=$({
      api --paginate "repos/$repo/actions/runs?per_page=100&created=%3E$since" --jq '.workflow_runs[]'
      api "repos/$repo/actions/runs?per_page=100" --jq '.workflow_runs[]'
    } | jq -sc --arg since "$since" 'unique_by(.id) | map(select(.status != "completed" or .updated_at > $since))')
    # Ids only on a command line: a page of runs is past what one argument
    # may hold.
    have=" $(jq -r '[.[].id] | join(" ")' <<<"$listed") "
    fetch=$(jq -r 'to_entries[] | select(.value == "running") | .key' <<<"$tracked" \
      | while read -r id; do [[ "$have" == *" $id "* ]] || echo "$id"; done)
  fi
  # Each run, cut to what is reported of it.
  all=$({
    jq -c '.[]' <<<"$listed"
    for id in $fetch; do api "repos/$repo/actions/runs/$id"; done
  } | jq -c '{id, name, status, conclusion, head_branch, event, display_title, html_url, updated_at, pull_requests: [.pull_requests[]? | {number}]}')
  while read -r run; do
    [[ -z "$run" ]] && continue
    id=$(jq -r .id <<<"$run")
    status=$(jq -r .status <<<"$run")
    if [[ -n "$runs" ]]; then
      named_items=$((named_items + 1))
      [[ "$status" == completed ]] && done_items=$((done_items + 1))
    fi
    was=$(jq -r --arg id "$id" '.[$id] // ""' <<<"$tracked")
    if [[ "$status" != completed ]]; then
      tracked=$(jq -c --arg id "$id" '.[$id] = "running"' <<<"$tracked")
      continue
    fi
    mark="done:$(jq -r .updated_at <<<"$run")"
    [[ "$was" == "$mark" ]] && continue
    tracked=$(jq -c --arg id "$id" --arg m "$mark" '.[$id] = $m' <<<"$tracked")
    # A run already complete at the baseline was not seen to complete.
    $first && continue
    # A named run complete and never seen running finished before this
    # instance began; the listing only offers others that completed since.
    [[ -z "$was" && -n "$runs" ]] && continue
    failed=""
    if [[ $(jq -r .conclusion <<<"$run") != success ]]; then
      failed=$(api "repos/$repo/actions/runs/$id/jobs?per_page=100" | jq -r '
        .jobs[] | select(.conclusion != "success" and .conclusion != "skipped")
        | "\(.name): \(.conclusion)\n" + ([.steps[]? | select(.conclusion == "failure") | "  step: \(.name)"] | join("\n")) + "\n  \(.html_url)"')
    fi
    jq -c --arg failed "$failed" '
      {kind: "run.completed", key: "run-\(.id)",
       title: "\(.name) \(.conclusion) on \(.head_branch)\(if (.pull_requests | length) > 0 then " (#" + ([.pull_requests[].number | tostring] | join(", #")) + ")" else "" end)",
       summary: "\(.event), \(.display_title // "")",
       url: .html_url, body: $failed}' <<<"$run"
  done <<<"$all"
  # The newest 200 reported runs are enough to recognize one seen again.
  tracked=$(jq -c '(to_entries | map(select(.value | startswith("done:"))) | sort_by(.value) | reverse | .[200:] | map(.key)) as $old
    | with_entries(select(.key as $k | $old | index($k) | not))' <<<"$tracked")
fi

# An instance watching only ids is finished when each is closed or complete.
if $only_ids && ((named_items > 0 && done_items == named_items)); then
  jq -nc --arg repo "$repo" '{kind: "github.finished", title: "everything this watches in \($repo) is closed or complete", final: true}'
fi

# The whole run succeeded: report it, then save where it got to.
exec 1>&3 3>&-
cat "$buffer"
echo "$items" >"$state/items.json"
echo "$tracked" >"$state/runs.json"
echo "$now" >"$state/since"
