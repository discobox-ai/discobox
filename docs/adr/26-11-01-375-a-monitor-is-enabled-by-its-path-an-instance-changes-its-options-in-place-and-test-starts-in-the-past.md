# 26-11-01-375 — A monitor is enabled by its path, an instance changes its options in place, and `test` can start in the past

- **Status**: Proposed
- **Date**: 2026-11-01
- **Amends**: [26-10-31-073](26-10-31-073-a-box-runs-the-monitors-its-agent-enables-and-queues-their-events-until-it-pops-them.md)
  §2's monitors only from two declaration directories and its refusal of an
  enable with other options, and §3's `test` that always starts at a
  baseline.
- **Relates to**: [0080](0080-the-image-ships-the-skills-for-what-it-installs.md),
  [26-10-09-395](26-10-09-395-a-sandbox-may-be-created-with-skills.md) and
  [26-10-14-294](26-10-14-294-a-skills-repository-is-a-source-its-skills-are-linked-from.md),
  the layers a box's skills arrive in.

## Context

A lead discobox running the `orchestrate-issues` skill watches its workers and
their pull requests with a polling loop the skill ships, `orchestrate.sh
watch`. The loop dies with the harness session and has to be restarted by hand
after every change to what it watches. Monitors (ADR 26-10-31-073) are meant
for exactly this, and mapping the loop onto them finds three gaps:

- **A skill cannot ship a monitor.** Monitors are read from the repository's
  `.discobox/monitors/` and the image's directory only (073 §2). The lead's
  watcher belongs to a skill that runs in any repository. Today it would be
  copied into each repository, or written there, uncommitted, each time the
  skill starts.
- **An instance's options cannot change.** The lead's watch list grows as its
  workers open pull requests. Enabling the same name with other options is
  refused (073 §2), so adding a PR means `disable` and `enable`. The new
  instance starts at a fresh baseline (073 §3), and whatever happened to the
  PRs it already watched in between is lost.
- **`test` cannot show what a monitor emits without a live change.** It runs
  from a fresh state directory, so its first run is a baseline and prints
  nothing (073 §3). `discobox-monitor test github-repo -o
  repo=discobox-ai/discobox --for 130s` printed `0 events`. That proves the
  monitor runs, not what its events look like.

## Decision

### 1. `enable` and `test` take a monitor by its path

`discobox-monitor enable <path>` and `discobox-monitor test <path>` run the
monitor script at that path, wherever it is. An argument with a `/` in it is a
path (`./watch.sh`, `/home/u/.claude/skills/lead/monitors/watch.sh`); any
other is an id, looked up in 073 §2's two directories as before.

- The file is read as one declaration, as a runner reads it before each run,
  and refused with its problem when it cannot run. The instance records its
  absolute path, cleaned but with links kept, so a skill linked in from a
  checkout (ADR 26-10-14-294) runs the checkout's file and sees its edits.
- The instance is named for the declaration's id unless `--name` says
  otherwise, and lists as that monitor.
- It runs as every monitor does (073 §3): as the sandbox user, in the
  repository root, or in the home directory in a box with no repository.

A skill ships a monitor by keeping the script beside its `SKILL.md`, say in
`monitors/`, and telling the agent to enable it by its path. A skill already
refers to its own files by paths relative to itself, which the agent resolves
wherever the harness installed the skill. Nothing is discovered, so nothing
about which directories a harness reads skills from is built into monitors,
and 073 §2's rule that nothing is enabled because it exists holds without
saying so.

`list` shows the monitors of the two directories only. A skill's monitors are
listed in the skill.

### 2. `enable --update` changes an enabled instance's options in place

`discobox-monitor enable <monitor> [--name N] --update [-o k=v]...` enables the
instance as `enable` does. When the name already holds that monitor, it
rewrites the instance as this enable resolved it instead of refusing: its
options, and the declaration it runs — the file an id resolves to now, or
the path given (§1).

- The options given replace the instance's options whole. A lead restates the
  repository along with the growing PR list. Merging per key would leave no
  way to remove an option.
- The instance keeps its ID and its state directory. To the reconciler and the
  runner it is the same instance, so no exec is replaced and nothing starts
  over at a baseline.
- A polling instance's next run gets the new options. A streaming instance's
  current run is ended and started again with them, since its options are
  in its environment.
- A run still going with the old options that ends the instance — a final
  event — does not end the updated one: the instance runs again with its new
  options. A final event says the old list is finished, not the new one.
- A name that holds a different monitor is still refused. That is a different
  instance, not new options for this one.
- The monitor is told nothing beyond its options and its state. What starts
  fresh is the monitor's to decide: a monitor that keeps state per target
  (per PR, per run) treats a target it has no state for as a baseline, so a
  newly named id reports only what happens after it was added. The
  `discobox-monitor` skill's template says so.

`enable` without `--update` is unchanged: other options are refused, and the
message names `--update` as well as `--name`.

### 3. `test --since DUR` starts from a baseline in the past, for a monitor that declares it can

A monitor declares `since: true` in its front matter when it can start from a
baseline at a given time instead of now. `discobox-monitor test <monitor>
--since DUR` gives the first run `DISCOBOX_MONITOR_SINCE`, the time `DUR` ago
as RFC 3339 UTC. Its baseline is then that time, so the first run prints what
happened since: real events, in the shape an enabled instance would queue.
Every later run is unaffected.

`test --since` on a monitor without `since: true` is refused before it runs,
naming the field. A monitor that ignored the variable would print nothing,
which reads as nothing having happened. The image's `github-repo` declares it.

Only `test` sets the variable. An enabled instance still starts at now (073
§3).

## Alternatives rejected

- **Discover a skill's monitors** from a `monitors/` directory in each
  installed skill, as a third directory between the repository's and the
  image's. It needs the list of directories every harness reads skills from
  built into monitors, a precedence rule between a repository, a skill and
  the image, and a rule for two skills declaring one id — and one unreadable
  skill directory fails every discovering command. A path asks for none of
  that: the skill already knows where its file is.
- **Copy a skill's monitors into `.discobox/monitors/` when the skill starts.**
  It leaves an uncommitted file in every repository the skill runs in. The
  copy never sees a fix made to the skill.
- **An `update <name>` verb.** It fails when the instance is not enabled yet,
  so a skill would have to check before choosing between `enable` and
  `update`. `enable --update` works either way and can be repeated.
- **`disable` and `enable` keeping the state directory.** The instance would
  have a new ID, so the reconciler would replace its exec. The gap between the
  two commands would still go unwatched.
- **Have `test` print the baseline as events.** A baseline holds where things
  stand (what is open, what last ran), not things that happened. Printing it
  needs a second output from every monitor and would not look like what an
  instance queues.
- **A declared sample event.** It shows the shape someone wrote down, not what
  the monitor actually emits against a real repository.

## Consequences

- `enable` and `test` accept a path. An instance's recorded path is what its
  runner loads, as before; only how it was resolved is new.
- `enable` gains `--update`, and the runner reads its instance from the
  instance file on each run.
- `test` gains `--since`; front matter gains `since`. `github-repo` (and its Go
  rewrite, #414) reads `DISCOBOX_MONITOR_SINCE` on a first run.
- A skill can ship the lead's watchers: `orchestrate-issues` can drop
  `orchestrate.sh watch` for instances it enables and updates by path.

## Deferred

- **`enable --since`**, to start an instance from a past baseline rather than
  now. Revisit when an agent needs to resume watching across a gap that no
  instance covered, for example replacing a loop that died, and `pop`
  after `enable` would otherwise miss it.
- **Listing the monitors of installed skills.** Revisit if agents miss a
  skill's monitors because its `SKILL.md` did not name them.
