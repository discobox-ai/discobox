---
name: discobox-monitor
description: Watch something without spending turns on it, and be told when it changes. Monitors queue events about a GitHub repository (issues, pull requests, Actions runs, one PR's reviews and checks), a command's output, a file's new lines or a URL until you pop them. You can write a new monitor as a small script. Use when you would otherwise poll by hand: waiting on CI, a review, a merge, a job elsewhere, a log line; or when the user asks to be told when something happens.
allowed-tools: Bash
---

# Watching with discobox-monitor

A **monitor** is a script that watches something and prints a line each time
it changes. **Enabling** one starts an **instance** of it. The instance keeps
running in this box, and the box holds its events in a queue until you
**pop** them. `pop` gives you a one-line header per event, and `read ID`
gives you all of one event.

```bash
discobox-monitor list                                 # monitors available, with their options
discobox-monitor enable github-repo -o repo=owner/name -o pr=412
discobox-monitor status                               # instances, their state, events pending
discobox-monitor pop                                  # take pending event headers
discobox-monitor read e17                             # all of one event: body, data, url
discobox-monitor disable github-repo                  # stop watching
```

## Start of a session: check what is already running

Instances belong to the box, not to your session. They keep running after
your harness exits or is relaunched, and after the box stops and starts. So
a fresh session may find instances and pending events it didn't start.
Before you begin:

```bash
discobox-monitor status
```

Pop what is pending and disable what you no longer need.

## Being woken: only a waiting `pop` wakes you

**Enabling a monitor watches. It does not wake you.** Discobox never types
into your terminal or prompts you. If you want an event to bring you back,
keep a waiting pop running in the background. It returns when an event
arrives, and that return is your wake-up:

```bash
discobox-monitor pop --wait          # until an event arrives
discobox-monitor pop --wait=30m      # or for at most 30 minutes
discobox-monitor pop --follow        # print each batch as it arrives, until interrupted
```

Run it the way your harness tells you a background command finished, for
example as a background shell command, or under a tool that streams a
command's lines (`--follow`). Without a waiting pop, events just wait until
the next time you run `pop`.

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

`discobox-box` reports what happens to other discoboxes: the ones you
started, or any in the project you can name. It reads the discobox API, so
this box must be allowed to call it (`discoboxApi`). Without that, every run
fails saying so, and only a person can allow it.

```bash
discobox-monitor enable discobox-box --name workers -o tag=issue           # every box tagged issue
discobox-monitor enable discobox-box --name workers -o 'tag=role=worker lead=me'  # every selector matches
discobox-monitor enable discobox-box --name w -o id=sbx_abc,sbx_def -o watch=terminal,requests
```

- `id` names boxes (comma-separated). `tag` selects them, including boxes
  created later: `key` or `key=value`, separated by spaces, all of which
  must match.
- `watch` picks kinds: `state`, `terminal`, `requests`. All three when
  omitted. `interval` polls more often than every 30s, down to 10s.
- What it reports:
  - **`box.state`:** a box's state changing (running, stopped, suspended,
    …). Also `box.created` for a selected box created after you enabled
    it, and `box.gone` for a named one that was deleted.
  - **`box.busy` / `box.idle`:** its primary terminal's program reporting
    that it is working, or idle, done, waiting (on a permission, a question,
    a login) or stopped on an error. This is what the program reports, not
    what its screen says.
  - **`box.request` / `box.request.answered`:** a secret request from it
    pending, and then approved or denied. You see requests only from the
    boxes this box created.
- An instance that names only ids ends itself once every one is gone.

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
