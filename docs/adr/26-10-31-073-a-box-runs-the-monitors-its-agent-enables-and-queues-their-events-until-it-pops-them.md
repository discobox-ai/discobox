# 26-10-31-073 — A box runs the monitors its agent enables, and queues their events until it pops them

- **Status**: Accepted
- **Date**: 2026-10-31
- **Relates to**: [ADR 0070](0070-services-are-declared-execs-the-sandbox-starts-for-you.md)
  and [ADR 0125](0125-tools-are-declared-in-files-the-way-services-are.md),
  the declared-by-file shape a monitor takes;
  [ADR 26-10-16-337](26-10-16-337-a-discobox-asks-to-sleep-and-names-when-to-wake.md),
  sleep and wake, which a monitor does not replace;
  [ADR 26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md),
  the terminal status a monitor does not act on.

## Context

An agent often has to keep an eye on something it does not control: a pull
request's checks and reviews, a CI run, a log, a URL, a long job in another
box. Today it either polls by hand — spending turns and context on "still
running" — or asks its harness to watch, which only some harnesses can do, each
its own way, and none of which keeps what it saw past the harness session.

What it needs is narrow: something that watches on its behalf, holds what it
saw until the agent asks, and hands it over in a form that costs little context
— a line per event, with the detail fetched only when wanted. And because the
agent is the one who knows what is worth watching, it has to be able to write a
new watcher as easily as it writes a shell script.

The sandbox already has the pieces. `pkg/declared` reads scripts with front
matter from an image directory and a repository's `.discobox/`; `execs`
supervises processes that outlive the agent; `discobox-power` showed that an
in-box CLI can be a personality of the agent that only renames files the agent
reads.

## Decision

### 1. `discobox-monitor` is the in-box CLI, a personality of the sandbox agent

One binary, named for what most of its verbs act on:

```
discobox-monitor list                                  # monitors available, with their options and problems
discobox-monitor show <monitor>
discobox-monitor enable <monitor> [--name N] [-o k=v]...
discobox-monitor disable <name>
discobox-monitor status [<name>]                       # running / backoff / failed / done, last event, stderr tail
discobox-monitor logs <name>
discobox-monitor pop  [--wait[=DUR]] [--follow] [--name N] [--max N] [--json]
discobox-monitor peek [same filters]
discobox-monitor read <id> [--json]
discobox-monitor emit --title T [--kind K] [--key K] [--body-file F]
discobox-monitor test <monitor> [-o k=v]... [--for DUR] # for writing a monitor; queues nothing
```

The working path is `enable`, then `pop`, then `read` when a header is not
enough. `test` runs a monitor in the foreground with its events on stdout and
nothing enabled or queued: it exists to check a monitor while writing it, and
its name says so. `emit` queues an event from anything — a build's last line, a
script — so work already running reports into the same queue.

It is a personality of `discobox-sandbox-agent` (as `discobox-power` is), so it
ships at the agent's version and reads the agent's own file formats through the
agent's own code.

Options are `-o key=value`, repeatable; `--option a=1,b=2` is accepted too, split
only on a comma followed by `ident=`, so a value may contain commas. An option
the monitor does not declare, or a required one missing, is refused before
anything is written, naming it.

### 2. A monitor is a declared script; an instance is an enabled monitor

A **monitor** is a script with front matter, read by `pkg/declared`:

```bash
#!/usr/bin/env bash
#---
# name: GitHub repository
# description: New activity on a repository's issues, pull requests and Actions runs
# interval: 60s
# options:
# - {name: repo, required: true, description: owner/name}
# - {name: watch, description: "issues, prs, actions; all three when omitted"}
# - {name: issue, description: "issue numbers to watch, comma-separated"}
# - {name: pr, description: "pull request numbers to watch, comma-separated"}
# - {name: run, description: "Actions run ids to watch, comma-separated"}
#---
```

Options are a list of one-line mappings because a commented front-matter
block loses its indentation (`frontmatter` strips what follows the `#`), so a
nested block mapping cannot be written in one.

An **instance** is a monitor enabled under a name with its options.
`--name` defaults to the monitor's id. Enabling the same monitor with the same
options under an existing name changes nothing; with different options it is
refused, saying to pass `--name`, so an instance is never replaced by accident.

A declared monitor does nothing until an instance of it is enabled. No monitor
starts because it exists.

Monitors are read from two directories, the repository's winning on a shared
id, as it does for services and tools:

1. the primary source's `.discobox/monitors/` — where an agent writes a new
   monitor;
2. the image's `/usr/local/share/discobox/monitors/` — the built-ins.

A monitor an agent writes goes in the repository, beside `.discobox/services`
and `.discobox/tools`: whether it is committed, kept uncommitted, or deleted is
the person's decision, made the way they make it for any other file. A file
that cannot be used is listed with its problem, never dropped.

### 3. A monitor prints JSON lines; only `title` is required

Each stdout line is one event:

```json
{"title":"PR #412: 2 of 14 checks failed","kind":"ci.failed","summary":"lint, test:windows",
 "url":"https://…","key":"ci","body":"…","data":{},"final":false}
```

- A line that is not a JSON object is the event `{"title": <line>}`, so
  `tail -F app.log | grep --line-buffered ERROR` is a monitor as written.
- `key` coalesces: a pending event from the same instance with the same key is
  replaced by the newer one, and the count of what it replaced is kept. A
  polling monitor cannot fill the queue with one condition restated.
- `final: true` ends the instance once that event is queued — a merged PR has
  nothing left to watch. An exit code means nothing beyond success or failure.
- stderr is the instance's log, never an event.

A monitor runs in one of two modes, chosen by front matter:

- **Polling** (`interval:` set): run once per interval, print zero or more
  events, exit. This is the mode the image's skill teaches first.
- **Streaming** (no `interval:`): run continuously; restarted with exponential
  backoff when it exits non-zero, `done` when it exits zero.

It runs as the sandbox user, in the repository root — the image's monitors
too, so a relative path or a `make` in an option means what it would in the
agent's own shell — or the home directory in a box with no repository, with
the box's egress and credentials as any command has them. Its environment carries `DISCOBOX_OPT_<NAME>` per option,
`DISCOBOX_MONITOR_OPTIONS` (all of them, as JSON), `DISCOBOX_MONITOR_NAME`, and
`DISCOBOX_MONITOR_STATE`: a directory of the instance's own that survives
restarts, for remembering what it already reported.

A monitor reports what happens after it is enabled, never what was already
there. Its first run — an empty state directory — records a baseline and
emits nothing; every later run reports changes against what it recorded. An
agent enabling a monitor is asking to hear what changes, and a backlog of a
repository's history would bury that in its first `pop`. `test` runs a
monitor in a fresh state directory of its own, so it starts at a baseline too:
what it prints is what an enabled instance would queue, and a first run that
prints anything is a monitor that does not keep to this.

### 4. Enabled instances are files, and the agent converges on them

`enable` renames `~/.discobox/monitors/enabled/<name>.json` (monitor id and
options) into place; `disable` removes it. The agent's `monitors` package
watches the directory and reconciles — level-triggered on fsnotify with a
periodic resync — the instances it runs to the files there. `ls` remains a
complete answer to what is on.

An instance runs as an exec, tagged `monitorId`/`monitorName` in exec metadata,
the way `services` runs a service: supervised by the same `UnitManager`, so it
outlives an agent restart. What the exec runs is the instance's **runner**, the
agent binary's `monitor-run` subcommand: it runs the monitor's script — once per
interval, or continuously with backoff — reads its stdout, and queues the
events itself (§6). The agent does not read a monitor's output: an exec's
output reaches the agent only as transcript rows written in batches (ADR 0028),
which has no position to resume from and a latency a waiting `pop` would feel.
The runner reads the pipe as the lines arrive, and because it outlives an
agent restart, so does the reading.

What the runner observes of its instance — state, last event, last error and
the tail of stderr — is written to `~/.discobox/monitors/state/<name>/`, which
`status` and `logs` read; `DISCOBOX_MONITOR_STATE` is a subdirectory of it the
monitor owns.

### 5. Instances belong to the box, not to a harness session

An enabled instance runs until it is disabled or sends a `final` event. It
survives the harness exiting and being relaunched, and a stop and start of the
box: at boot the agent starts every instance whose file is there, and a polling
monitor catches up from its state directory. The queue survives with it.

So a fresh harness session can inherit running instances and pending events it
did not start. The image's skill tells the agent to run `discobox-monitor
status` when it begins, and to disable what it no longer needs.

### 6. Events are files; `pop` is a rename

The runner queues each event as `~/.discobox/events/pending/<id>.json`, written
under a dot name and linked into place under its id, so two writers can never
take one id. `pop` renames what it returns into
`~/.discobox/events/read/`, so two concurrent pops never return one event, and
`read <id>` finds an event in either directory — the detail is still there
after the header was popped. `read/` is pruned by age and count.

Ids are short and monotonic per box (`e17`): the agent has to type them back.

`pop` prints one header per event — id, age, instance, kind, title, summary
truncated, and the size of the body — then a count of what is left and the
`read` command, or JSON lines with `--json`. A body over 1 MiB is cut there,
and the event says so.

The runner queues events of its own about its instance: `monitor.failed` when
it starts failing (with the stderr tail — a 401 from a credential the box was
not given reaches the agent this way), `monitor.recovered` when it stops, and
`monitor.dropped` when the instance's pending events reached their cap and the
oldest were dropped. The cap is what bounds a flooding instance.

### 7. Discobox never prompts the agent; a waiting `pop` is how it is woken

Nothing in this design types into a terminal, injects a prompt, or blocks a
harness's turn from ending. An agent that wants to be woken by an event keeps
a `discobox-monitor pop --wait` running — in a harness that resumes a turn
when a background command exits, or as `pop --follow` under a harness tool
that streams a command's lines. When the event arrives the waiting `pop`
returns it, and that is the wake.

The image's skill says this plainly: **enabling a monitor watches; only a
running `pop --wait` wakes you.** An agent with no `pop` waiting sees its events
the next time it pops.

### 8. A monitor does not hold the box up

An instance holds no keepalive lease, and its events are not activity: a box
that only watches still goes idle and stops (ADR 0108), and an agent may still
sleep it (ADR 26-10-16-337). Instances resume at the next boot (§5). Where a
freeze keeps the box's memory, a waiting `pop` is still waiting when it thaws;
after a shutdown it is gone, and the agent relaunched with `DISCOBOX_WAKE`
should pop before anything else. To look again at a time while asleep, the
agent pairs a monitor with a wake.

### 9. The image ships built-ins and a skill

`/usr/local/share/discobox/monitors/` carries `github-repo` (below),
`command` (run `cmd` every `every`, emit when its output changes), `file`
(lines of a file matching a pattern) and `url` (a status or body change). A
`discobox-monitor` skill in the core feature's skills teaches using them,
writing a polling monitor from a template into `.discobox/monitors/`, testing
it with `test`, and §7.

**`github-repo`** watches one repository. `repo` is required; everything else
narrows it:

```
discobox-monitor enable github-repo -o repo=foo/bar                          # issues, PRs and Actions runs
discobox-monitor enable github-repo -o repo=foo/bar -o watch=prs,actions
discobox-monitor enable github-repo --name pr-412 -o repo=foo/bar -o pr=412  # only PR 412
discobox-monitor enable github-repo -o repo=foo/bar -o pr=412 -o watch=prs,actions   # PR 412, every run
```

- `watch` picks the kinds — `issues`, `prs`, `actions` — and is all three when
  omitted and no ids are given.
- `issue`, `pr` and `run` narrow their kind to those ids. Given without
  `watch`, the ids also choose the kinds: `-o pr=412` watches PR 412 and
  nothing else, which is what an agent naming one PR means. With `watch`, a
  kind that has ids is narrowed and a kind without is watched whole.
- Only new activity is reported (§3): opened, closed, reopened, merged and
  ready for review; comments, and reviews with their inline comments; for a
  PR named by id, its head's checks reaching a conclusion; for Actions, a run
  completing — including a run already in progress when the instance was
  enabled, since its completion has not happened yet. Repository-wide, a
  PR's CI is its Actions runs, which name the PR they ran for: following
  every open PR's checks would cost a request per PR per poll.
- Each event's `kind` is `issue.*`, `pr.*` or `run.*`; its `key` is the object
  and aspect (`pr-412-checks`), so a PR whose checks flap leaves one pending
  event. Its `body` is the comment or review text, or a failed run's failed
  jobs and steps; its `url` is the page on GitHub.
- It reads the REST API through `gh` with the box's credential, each run
  reading what happened between the last run and this one in GitHub's own
  clock, so consecutive runs neither miss nor repeat anything whatever the
  box's clock says. It does not use the repository
  events feed: that feed carries no Actions runs or checks, and lags by
  minutes to hours. Without a credential it fails, and the `monitor.failed`
  event (§6) names `discobox-access` as the way to ask for one.
- An instance watching only ids ends itself (`final`, §3) once every issue
  and PR it names is closed or merged and every run it names has completed.

## Alternatives rejected

- **An MCP server.** Each harness wires MCP differently, and some not at all;
  every harness runs shell commands. A CLI with a skill reaches all of them.
- **The harness's own background watching.** Not every harness has it, what it
  has dies with the session, and nothing survives a relaunch or a sleep. It
  remains a good consumer of `pop --wait` and `pop --follow` (§7).
- **Discobox waking an idle agent by typing a prompt into its terminal.** It
  would type into a terminal a person may be using, its text would be one more
  thing every harness reads differently, and it takes the decision of what to
  attend to away from the agent. A waiting `pop` gives the same wake to an agent
  that asked for it, and to no one else.
- **Blocking the harness's `Stop` hook while events are pending.** A softer form
  of the same thing: the agent ends its turn and discobox overrides it.
- **An agent HTTP route for enable and pop.** Files renamed into place are the
  in-box convention `discobox-power` set; they are atomic without a server,
  readable with `ls` and `cat`, and leave nothing to authenticate.
- **Enabling every declared monitor automatically.** A repository's monitors
  would then run in every box of it, for whatever agent is there, watching
  things nobody asked about — with credentials, egress and context spent on
  it. A monitor is a template; the agent decides what to watch (§2).
- **Instances scoped to a harness session.** A relaunch after a shutdown —
  the moment a wake matters — would lose exactly the watching the agent set up
  before it slept (§5).
- **A box-local directory for written monitors.** It hides a file the agent
  wrote from the person whose repository it is working in; the repository's
  `.discobox/monitors/` puts it where they see it and decide.
- **Naming it `discobox-events`.** Every verb but `pop`, `peek`, `read` and
  `emit` acts on a monitor, as `discobox-power`'s act on power.

## Consequences

- An agent can watch a repository, one PR, a CI run or a log without spending turns on it, and
  writes a new watcher as a shell script in the repository.
- `pkg/declared` gains a third reader; `.discobox/` a third directory beside
  `services` and `tools`.
- A fresh session may find instances and events it did not start (§5); the
  skill makes `status` its first step.
- Nothing watches while the box is down. An event that matters while asleep is
  seen only at the next wake.
- Monitors run with the box's credentials: a monitor in a repository can reach
  whatever the agent was granted, as any script it runs can.

## Deferred

- **Watching while the box is down.** Running a monitor pool-side, so an event
  can start a sleeping box, needs a credential outside the box and a place to
  run untrusted repository code. Revisit when a pool can run per-sandbox work
  with a binding of its own.
- **A notice through the harness's hooks.** `discobox-hook-publish` could add
  "3 events pending" to an active turn's context. Deferred until a hook can
  return context on every harness that publishes it; it would be a notice
  during a turn, never a prompt (§7).
- **The console.** A pending count on the sandbox's status and a read-only
  listing of instances. Revisit once the in-box surface has settled.
