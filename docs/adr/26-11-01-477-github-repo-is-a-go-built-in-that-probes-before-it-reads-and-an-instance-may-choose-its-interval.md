# 26-11-01-477 — `github-repo` is a Go built-in that probes before it reads, and an instance may choose its interval

- **Status**: Proposed
- **Date**: 2026-11-01
- **Amends**: [26-10-31-073](26-10-31-073-a-box-runs-the-monitors-its-agent-enables-and-queues-their-events-until-it-pops-them.md)
  §3's polling interval "chosen by front matter", and §9's `github-repo`:
  its reading the REST API through `gh`, its options, and its reason for not
  following every PR's checks.
- **Relates to**: 26-11-01-375 (proposed in pull request #428), which amends
  073 §§2–3 for the rest of issue #427 and touches nothing here.

## Context

`github-repo` (073 §9) was a bash and `jq` script polling the REST API through
`gh`. Measured against discobox-ai/discobox (#414):

- A quiet repository cost about 7 calls a minute per instance, out of the
  5,000 an hour the agent's `gh` shares.
- Reviews cost one call per updated PR, plus one per review.
- A run queued nothing unless all of it succeeded, and nothing waited out a
  rate limit. A long enough gap retried the same growing window every poll
  and never got through it.

`gh api` treats a `304` as an error, so the script could not send a
conditional request at all. A lead discobox also needs to follow PRs it
cannot name yet (its workers'), one branch's CI, and a PR starting to
conflict with its base (#427).

## Decision

### 1. A monitor written in Go is a declared script that execs the agent

`github-repo` is still `10-github-repo.sh`, with the same id, front matter
and options. Its body is `exec discobox-sandbox-agent monitor-builtin
github-repo`. That subcommand runs the Go implementation the agent ships,
handing it the instance the runner described (`DISCOBOX_MONITOR_*`). It prints
events as the lines 073 §3 defines, and fails by exiting non-zero with the
reason on stderr.

Every part of 073 stands for it:

- the runner and its failure and recovery events;
- the state directory;
- the queue;
- `list`, `show`, `test`.

`monitors/builtin` is the seam: a built-in is one run of a Go function given
the instance's environment. #427's `discobox-box` is the next one on it.

### 2. A polling monitor may let each instance choose its interval

A polling monitor that declares an option named `interval` runs each instance
at the duration that instance gives it. It is bounded below by the front
matter's new `min-interval` (at least 1s), and the declared `interval` stays
the default. `enable` refuses a value below the bound, and the runner reads
the option again before each run.

`github-repo` declares a 60s default and a 10s floor. GitHub's secondary
limits start to apply around 10s. A probe that costs nothing (§3) makes a
short interval for one PR reasonable, but not for a whole repository.

### 3. `github-repo` probes before it reads

- **Probes are conditional.** Each run begins with GETs sent with the last
  answer's ETag:
  - the issue updated last;
  - the newest 100 Actions runs, narrowed by `branch`;
  - each watched PR's head check runs and commit statuses;
  - each run followed by id.

  A `304` is not counted against the rate limit, so a quiet repository costs
  nothing. A probe's new ETag is saved only once what its change calls for is
  done.
- **What changed is read by GraphQL.** A search for issues and PRs updated
  since the last pass reads their comments, state changes and reviews nested
  in it, oldest first, a page at a time, each saved as it is done. One aliased
  query reads the watched PRs' checks (`statusCheckRollup`) and `mergeable`.
  REST is left for Actions runs and a failed run's jobs.
- **Rate limits are waited out.** `X-RateLimit-*`, `Retry-After`, secondary
  limits, GraphQL's `RATE_LIMITED` and `X-Poll-Interval` all set a time
  before which runs call nothing. A run that stops for one keeps what it did,
  and the next run resumes.
- **The credential** is `GH_TOKEN` or `GITHUB_TOKEN`, else `gh auth token`.
  Without one, the failure still names `discobox-access`.

### 4. `head`, `author`, `branch` and `pr.conflict`

- `head` (a branch glob) and `author` select PRs the way `pr` names them,
  including PRs opened after the instance was enabled. A selected PR, like a
  named one, reports `pr.checks`.
- `branch` narrows Actions runs to one branch.
- `pr.conflict`, keyed `pr-N-mergeable`, is reported when a watched PR's
  `mergeable` becomes `CONFLICTING`, and again when it clears. `UNKNOWN`,
  which GitHub returns until it has computed the value, is not a change.
- Repository-wide, a PR's CI is still its Actions runs (073 §9), now for a
  different reason. Reading every open PR's rollup is one query per change,
  so cost no longer rules it out. But `pr.checks` for every PR would repeat
  each `run.completed`.

## Alternatives rejected

- **A second kind of monitor, run by the reconciler without a script.** It
  would need its own declaration, listing and runner. The script keeps the
  declaration as the one place a monitor's name, options and interval are
  written, and the runner as the one thing that runs monitors.
- **A streaming built-in that loops on its own.** A runner sees a streaming
  monitor fail only when it exits, and reports it recovered only when a run
  succeeds, which a monitor that runs forever never does. A credential
  granted after a failure would leave the instance `failing` for good.
- **Polling at the 10s floor and skipping runs until an instance's own
  interval.** Every instance would start a process every 10 seconds to do
  nothing, and `status` would count runs that never happened.
- **`repository.pullRequests` instead of the search, to avoid the search
  index's lag.** It has no `since`, and it orders by update only newest
  first, so a pass cannot save its progress oldest first. The search's lag is
  covered by searching an overlap before the last pass, with the items
  already reported held by id, and by passes going on after a change until
  one that began that overlap after it has completed.
- **Keeping the bash script and adding `--include` to `gh api` for ETags.**
  It fixes only the idle cost. Per-item `jq` processes, a catch-up with no
  saved progress, and nested reads would remain.

## Consequences

- An idle `github-repo` instance costs no rate limit. A change costs one
  GraphQL query a page, plus REST for runs.
- The image's `github-repo` no longer needs `gh` or `jq`. They stay for the
  other monitors and for the agent.
- Any polling monitor can let its instances choose an interval. The image's
  `command` and `url` keep their own `every` loops for now; converting them
  would change instances already enabled with `every`.

## Deferred

- **Webhooks.** A GitHub App whose events the control plane receives and
  fans out to subscribed boxes is the only design that can wake a sleeping
  box. Revisit with an ADR of its own once the control plane can receive
  events from a GitHub App on a box's behalf.
