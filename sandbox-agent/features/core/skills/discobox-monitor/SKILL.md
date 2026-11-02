---
name: discobox-monitor
description: Watch something and be woken when it changes, by running `discobox-monitor pop --follow` under Claude Code's Monitor tool. Read this before you write a watch or poll of your own (a Monitor script, `tail -f`, an `until`/`sleep` loop, `gh pr checks --watch`). An instance may already be watching it (`discobox-monitor status`), and built-ins cover a GitHub repository (issues, PRs, reviews, checks, Actions runs), a command's output, a file's new lines and a URL. Use when waiting on CI, a review, a merge, a deploy or a log line, or when the user asks to be told when something happens.
allowed-tools: Bash, Monitor
---

# Watching with discobox-monitor

A **monitor** is a script that watches something and prints a line each time
it changes. **Enabling** one starts an **instance** of it. The instance keeps
running in this box, and the box holds its events in a queue until you
**pop** them. In Claude Code, the **Monitor tool** runs the pop, so each
event arrives in your conversation as a notification.

Those are two separate jobs. The instances do the watching, and one
Monitor tool streams their events to you. Don't put the watching itself in
a Monitor script: no `tail -f`, no `while sleep` loop, no `gh pr checks`
poll.

```bash
discobox-monitor status                               # instances, their state, events pending
discobox-monitor list                                 # monitors available, with their options
discobox-monitor enable github-repo -o repo=owner/name -o pr=412
discobox-monitor pop                                  # take pending event headers
discobox-monitor read e17                             # all of one event: body, data, url
discobox-monitor disable github-repo                  # stop watching
```

## Before you write any watch of your own

Go down this list and stop at the first step that fits:

1. **Something is already watching it.** Run `discobox-monitor status` on
   its own, and read it before you enable anything. Don't chain an `enable`
   after it. If an instance covers what you want, even roughly, use it,
   and keep the Monitor tool streaming its events (next section). Don't
   enable a duplicate, and don't disable an instance you didn't start: it
   may be the user's, or another session's. To make it watch more, run
   `enable --update` with the options it already has, plus yours.
2. **A monitor exists for it.** `discobox-monitor list` shows the built-ins
   and the repository's `.discobox/monitors`. The built-ins cover a GitHub
   repository, a PR, a CI run, a command's output, a file's new lines and a
   URL. Enable one, then stream it.
3. **Nothing fits, and it's worth keeping.** Write a monitor
   (`Writing a monitor`, below), test it, enable it, then stream it.
4. **Only then write an ad-hoc watch,** and only for something short-lived
   that you started yourself, such as a build you launched in this
   session. For that, a background Bash command that exits when it's done
   is fine.

Why monitors beat a script of your own:
- The queue holds events while nothing is streaming them, so a Monitor
  tool expiring, a harness restart or the box sleeping loses nothing.
- The user and the next session can see what's being watched.
- `github-repo` costs no rate limit on a quiet repository. It reports
  reviews, checks, conflicts and failed steps, which a quick script misses.

## In Claude Code: one Monitor tool runs the pop

Start one Monitor tool on the whole queue, not one per instance:

```
Monitor(
  command: "discobox-monitor pop --follow",
  description: "discobox monitor events",
  timeout_ms: 1800000,
)
```

- Give it `timeout_ms: 1800000`, the most the Monitor tool allows, so you
  re-arm it as rarely as possible.
- Each notification line is one event header, with its id, instance, kind
  and title. Run `discobox-monitor read ID` for its body: failed jobs,
  review comments, log excerpts.
- **Re-arm it when it expires.** The Monitor tool stops after at most 30
  minutes. While you are still waiting, start the same command again.
  Events that arrived in between are still queued, and the new pop
  delivers them first.
- **Run only one pop at a time.** Each event goes to whichever pop takes it
  first. A second `--follow` takes events away from the first, and so does
  a stray `pop`. If a Monitor tool is already running the pop, enabling
  another instance needs nothing more: its events come through the same
  stream.
- **A box shared by sessions.** A box normally runs one agent, so stream
  the whole queue. When you know other sessions in this box watch too (a
  lead and its helpers, the user's second terminal), every session streams
  only the instances it started or was handed, one Monitor tool per
  instance (`pop --follow --name pr-412`). Each one looks at the rest with
  `peek`, which takes nothing. A whole-queue pop would take their events.
- **Keep the stream quiet.** The Monitor tool stops a command that prints
  too much, and events popped while it was suppressing output are no
  longer pending, so nothing re-delivers them. Narrow a busy instance
  (`watch`, ids, `match`) rather than streaming everything. If the tool
  stopped the stream, `read` still shows each event by its id. Ids count up
  (`e17`, `e18`, …), so read on from the last one you were shown.
- To stop being woken, stop the Monitor tool. Disable the instance only
  when nothing should watch it any longer.

**Only a running pop wakes you. Enabling a monitor does not.** Discobox
never types into your terminal. Without a running pop, events wait until
you next run `pop`. In a harness without a Monitor tool, run
`discobox-monitor pop --wait` as a background command (`--wait=30m` bounds
it), and start it again after each wake.

## Start of a session: check what is already running

Instances belong to the box, not to your session. They keep running after
your harness exits or relaunches, and after the box stops and starts. A
fresh session may find instances and pending events it didn't start. Run
`discobox-monitor status` before you begin. Pop what is pending. If you
are still waiting on something, start the Monitor tool. Leave instances
you didn't start alone unless the user says otherwise. In a box shared by
sessions, pop only your own instances (`--name`) and `peek` the rest, here
and after a wake.

Monitors don't keep the box up, and their events don't count as activity.
A box that is only watching still goes idle and stops. Instances resume at
the next boot. To look again later while the box is asleep, pair the
monitor with a wake (`discobox-power sleep --wake-in 30m`, see the
`discobox` skill). After a wake, pop before anything else.

## The built-in monitors

`github-repo` reports new activity in one repository. It uses the box's
GitHub credential (`GH_TOKEN`). If it fails for want of one, or with 401 or
403, ask for GitHub access with `discobox-access`. A quiet repository costs
it no rate limit, so leave it enabled.

```bash
discobox-monitor enable github-repo -o repo=foo/bar                            # issues, PRs and Actions runs
discobox-monitor enable github-repo -o repo=foo/bar -o watch=prs,actions
discobox-monitor enable github-repo --name pr-412 -o repo=foo/bar -o pr=412    # only PR 412
discobox-monitor enable github-repo --name ci -o repo=foo/bar -o run=4114819   # one Actions run
discobox-monitor enable github-repo --name workers -o repo=foo/bar -o 'head=discobox/*'  # PRs from these branches
discobox-monitor enable github-repo --name main-ci -o repo=foo/bar -o branch=main        # main's CI only
```

- `watch` picks kinds: `issues`, `prs`, `actions`. Without `watch`, ids or
  filters, all three are watched.
- `issue`, `pr` and `run` narrow their kind to those ids (comma-separated).
  Given without `watch`, the ids also pick the kinds: `-o pr=412` watches
  PR 412 and nothing else.
- `head` (a branch glob; `*` does not cross a `/`) and `author` (a login)
  select PRs the way `pr` names them, including ones opened later. Use them
  for PRs you can't name yet. `branch` narrows Actions to one branch's runs.
- `interval` polls more often than every 60s, down to 10s. Use it for one PR
  you are waiting on, not a whole repository.
- What it reports:
  - **Issues and PRs:** opened, closed, merged, reopened, ready for review.
  - **Comments and reviews:** comments, and reviews with their inline
    comments.
  - **A PR named by id or selected by `head`/`author`:** its checks passing
    or failing (`pr.checks`), and `pr.conflict` when it starts conflicting
    with its base, the cue to rebase, and again when it no longer does.
  - **Actions:** runs completing, with a failed run's failed jobs and steps.
- An instance that watches only ids ends itself once each of them is closed
  or complete.

The other built-ins:
- **`command`:** `-o cmd='make status' -o every=2m` runs the command and
  reports when its output or exit status changes.
- **`file`:** `-o path=log/app.log -o match='ERROR|panic'` reports new
  lines.
- **`url`:** `-o url=https://… -o every=5m` reports when the status or body
  changes.

Each monitor reports only what happens after you enable it, never a
backlog.

One instance per name. Enabling the same monitor with the same options
again does nothing. To watch two PRs, give each its own `--name`, or list
both in one instance (`-o pr=412,413`).

To change what an instance watches, enable it again with `--update`. The
options you give replace its options whole, so restate the ones you keep.
The instance keeps what it already saw: nothing it was watching starts over,
and only a newly named id starts from now. It also picks up the monitor's
current script, or the path you name.

```bash
discobox-monitor enable github-repo --name lead --update -o repo=foo/bar -o pr=412,413,415
```

`--update` works whether or not the instance is enabled yet, so a script can
run the same command every time.

## Reading events

```
$ discobox-monitor pop
e17  4m ago    pr-412  pr.checks  #412 checks failed on 3f2a91c: lint, test  [x2, https://…, body 1 KB]
e18  just now  pr-412  pr.review  alice requested changes on #412 — this leaks the fd  [https://…, body 2 KB]
2 popped, 0 pending. 'discobox-monitor read ID' shows one whole.
```

- `xN` means newer events with the same key replaced older ones you hadn't
  popped yet. The event you see is the latest.
- `read` still works after a pop. Popped events are kept for a while.
- `peek` shows headers without taking them. `--name` narrows to one
  instance. `--json` gives JSON lines.
- `monitor.failed` means an instance started failing. It includes the end
  of its stderr. `discobox-monitor logs NAME` shows all of it.
  `monitor.recovered` follows when it works again.
- `discobox-monitor emit --title 'tests done' --body-file out.txt` queues an
  event from anything, such as the end of a long build.

## Writing a monitor

Put a new monitor in the repository, in `.discobox/monitors/` at the root of
the primary source. The user decides whether to commit it or delete it.
Tell them you wrote it.

A monitor can also live anywhere and be named by its path: any argument
with a `/` in it is a path. That is how a skill ships one, for watching
that belongs with the skill rather than with one repository: it keeps the
script beside its `SKILL.md` and says to enable it by its path relative to
the skill. `list` doesn't show monitors named by path.

```bash
discobox-monitor test <skill dir>/monitors/watch.sh -o repo=foo/bar
discobox-monitor enable <skill dir>/monitors/watch.sh --name lead --update -o repo=foo/bar
```

A monitor is an executable script with front matter:

```bash
#!/usr/bin/env bash
#---
# name: Deploy status
# description: Reports when the staging deploy's version changes
# interval: 60s
# options:
# - {name: env, required: true, description: the environment to watch}
# - {name: verbose, description: include the full manifest in each event}
#---
set -euo pipefail
state="$DISCOBOX_MONITOR_STATE/version"
now=$(curl -fsS "https://deploy.example/api/$DISCOBOX_OPT_ENV/version")
if [[ -f "$state" && "$now" != "$(<"$state")" ]]; then
  jq -nc --arg v "$now" --arg env "$DISCOBOX_OPT_ENV" \
    '{kind: "deploy.changed", key: "version", title: "\($env) is now \($v)"}'
fi
echo "$now" >"$state"
```

Rules:

- **Front matter.**
  - Lines between `#---` markers. `name` and `description` are what `list`
    shows.
  - Each option goes on its own `- {name: …, required: …, description: …}`
    line. The block loses its indentation, so a nested mapping won't parse.
  - Quote a description that has a comma in it (`description: "a, b"`):
    in a one-line mapping an unquoted comma starts the next field.
- **Two modes.**
  - **Polling:** with `interval:` (at least `1s`), the script runs once per
    interval and exits. Start with this one. To let each instance choose
    how often it runs, declare an option named `interval` (`-o
    interval=15s`) and, if it needs one, a floor (`min-interval: 10s`). The
    runner then paces it. Don't write a sleep loop for that.
  - **Streaming:** without `interval:`, the script runs continuously. Use
    it for `tail -F` and loops. If it exits non-zero, it's restarted with
    backoff. If it exits zero, the instance is done.
- **Environment.**
  - `DISCOBOX_OPT_<NAME>` holds each option, upper-cased with `-` turned
    into `_`.
  - `DISCOBOX_MONITOR_OPTIONS` holds all options as JSON.
  - `DISCOBOX_MONITOR_NAME` is the instance's name.
  - `DISCOBOX_MONITOR_STATE` is a directory that survives restarts, for
    remembering what you already reported.
  - Every monitor, the built-ins included, runs in the repository root, so
    relative paths in options are relative to it.
- **Only what changed.** If the state directory is empty, this is the
  first run: record a baseline and print nothing. Report changes against it
  after that. If you keep state per target (per PR, per run), treat a
  target with no state as a baseline too. That way an id added with
  `--update` reports only what happens after it was added.
- **Starting in the past (optional).** Add `since: true` to the front
  matter if the monitor can take its baseline at a given time. A first run
  with `DISCOBOX_MONITOR_SINCE` set (RFC 3339, UTC) should report what
  happened after that time instead of printing nothing. Only `test --since`
  sets it.
- **Output.**
  - Each stdout line is an event. A JSON object needs only `title`. It can
    also carry `kind`, `summary`, `url`, `key`, `body` (the detail `read`
    shows, up to 1 MiB), `data` (any JSON) and `final`.
  - A plain line becomes an event whose title is the line.
  - Give a recurring condition a `key`, so an unpopped event is replaced
    instead of piling up.
  - Print `"final": true` when there is nothing left to watch. That ends
    the instance.
- **stderr** goes to `discobox-monitor logs NAME`, not the queue. A
  non-zero exit is reported once as `monitor.failed`.
- **Executable.** Run `chmod +x`. `discobox-monitor list` names anything
  wrong with a monitor.

Test a monitor before you enable it. `test` runs it in a fresh state
directory and prints its events as JSON lines, with nothing enabled or
queued. A correct monitor prints nothing on its first run.

```bash
discobox-monitor test deploy-status -o env=staging --for 3m
```

To see what its events actually look like, start a monitor that declares
`since: true` from a past baseline, such as `github-repo`. Its first run
then prints what really happened in that window:

```bash
discobox-monitor test github-repo -o repo=foo/bar --since 2h --for 10s
```
