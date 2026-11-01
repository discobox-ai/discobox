# 26-11-01-375 — Skills declare monitors, an instance changes its options in place, and `test` can start in the past

- **Status**: Proposed
- **Date**: 2026-11-01
- **Amends**: [26-10-31-073](26-10-31-073-a-box-runs-the-monitors-its-agent-enables-and-queues-their-events-until-it-pops-them.md)
  §2's two declaration directories and its refusal of an enable with other
  options, and §3's `test` that always starts at a baseline.
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

### 1. A skill declares monitors in its `monitors/` directory

A skill directory may hold `monitors/`, read as 073 §2's directories are: a
script with front matter per monitor, each listed, its problem included. Any
installed skill can declare monitors: the image's, the repository's, the
sandbox's own and a skills source's. The sandbox reads the skills that are
installed, not the layers they came from. For each name in the run user's
skill directories (`~/.claude/skills`, then `~/.agents/skills`; the first
directory holding a name is the one read), `<skill>/monitors/` is a
declaration directory. A skill that a skills source links in is read through
its link, so an edit to the checkout reaches its instances, as 073's rules
say an edit to a declaration does.

Monitors are now read from three places. On a shared id, the first wins:

1. the primary source's `.discobox/monitors/`;
2. the installed skills' `monitors/`;
3. the image's `/usr/local/share/discobox/monitors/`.

The repository wins over a skill because `.discobox/monitors/` is where 073 §2
tells an agent to write a monitor. An agent fixing a skill's monitor for this
repository writes it there and expects that copy to run. A skill wins over the
image because a skill shipping a monitor with a built-in's id means to replace
it.

Two skills declaring one id are both listed, each with a problem naming the
other, and neither can be enabled. Nothing orders two skills, so letting the
name that sorts first win would only look like a rule.

A skill's monitor runs as the image's do (073 §3): as the sandbox user, in the
repository root, or in the home directory in a box with no repository. The
listing and `show` name the skill a monitor came from.

073 §2's rule stands: nothing is enabled because it is declared. A skill that
wants its monitor running tells the agent to enable it.

### 2. `enable --update` changes an enabled instance's options in place

`discobox-monitor enable <monitor> [--name N] --update [-o k=v]...` enables the
instance as `enable` does. When the name already holds that monitor with
other options, it rewrites the instance's options instead of refusing.

- The options given replace the instance's options whole. A lead restates the
  repository along with the growing PR list. Merging per key would leave no
  way to remove an option.
- The instance keeps its ID and its state directory. To the reconciler and the
  runner it is the same instance, so no exec is replaced and nothing starts
  over at a baseline.
- A polling instance's next run gets the new options. A streaming instance's
  current run is ended and started again with them, since its options are
  in its environment.
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

- **Copy a skill's monitors into `.discobox/monitors/` when the skill starts.**
  It leaves an uncommitted file in every repository the skill runs in. The
  copy never sees a fix made to the skill.
- **Read monitors from skills sources only.** Skills sources are how a team
  ships skills it edits, but the image's and the repository's skills are
  skills too, and the harness reads them all from one place. That place is
  what this reads.
- **Namespace a skill's monitors (`orchestrate-issues/discobox-box`).** Ids
  would never collide, but a repository could no longer replace a skill's
  monitor by writing one of the same id (§1). An agent reading a skill would
  also have to type a name unlike any other monitor's.
- **Let the skill's monitor win over the repository's.** It matches the skills
  layering, where the sandbox's own skills win over the repository's (ADR
  26-10-09-395 §3). But an agent writing a repository monitor to replace a
  skill's one would then find it ignored.
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

- `monitors.Discover` takes the run user's skill directories as well. A
  definition and an instance name the skill they came from.
- `enable` gains `--update`, and the runner reads its instance's options from
  the instance file on each run.
- `test` gains `--since`; front matter gains `since`. `github-repo` (and its Go
  rewrite, #414) reads `DISCOBOX_MONITOR_SINCE` on a first run.
- A skill can ship the lead's watchers: `orchestrate-issues` can drop
  `orchestrate.sh watch` for instances it enables and updates.

## Deferred

- **`enable --since`**, to start an instance from a past baseline rather than
  now. Revisit when an agent needs to resume watching across a gap that no
  instance covered, for example replacing a loop that died, and `pop`
  after `enable` would otherwise miss it.
- **Features declaring monitors** beside the skills they install. An image
  feature already installs into the image's directory. Revisit if a feature
  needs a monitor that a box without the feature must not list.
