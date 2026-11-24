# 26-11-24-336 — A provisioning step reports its progress, and the agent counts what the box receives

- **Status**: Proposed (would extend [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §6's `provision.json` with a running step's progress, and §3's `run` step
  with a channel to report it on)
- **Date**: 2026-11-24
- **Issue**: [#1572](https://github.com/discobox-ai/discobox/issues/1572)
- **Relates to**: [26-11-23-004](26-11-23-004-a-snapshot-build-reports-its-progress-and-an-admin-reaches-it-through-its-pool.md)
  §1, which streams `provisionspec.Status` out of a snapshot build, so what
  this adds reaches a build's report too;
  [26-11-03-815](26-11-03-815-a-pool-waits-on-a-sandboxs-ports-and-reports-a-change-at-once.md)
  §§2–3, the status wait that carries a change of `provision.json` to the
  pool.

## Context

At `e6f5ea44`, creating a box on a slow network shows one line for minutes:

```
provisioning claude-code / cli — waiting 2m50s
```

What the box knows, and what it says:

- **A feature's status is its state and its step's name.**
  `provisionspec.FeatureStatus` (`pkg/provisionspec/status.go`) carries
  `state`, `how`, `version`, `async`, `step`, `log`, `reason` and `error`.
  Nothing in it moves while a step runs.
- **A step's output goes to its log, and nowhere else.** `runCommand`
  (`sandbox-agent/provision/runner.go`) sends a script's stdout and stderr to
  the feature's log under `/var/log/discobox/provision/`. The tracker
  (`sandbox-agent/provision/tracker.go`) hears of a step only as it starts
  (`step`) and as its feature finishes (`finish`).
- **The CLI renders the step's name and the time.** `agentPreparation`
  (`cli/internal/sandboxcreate/provisionstatus.go`) makes
  `provisioning <feature> / <step>` of each blocking feature in progress, and
  `watchProvisioning` (`cli/internal/cli/provisionwatch.go`) appends
  `— waiting <elapsed>`. `sandboxProvisioning`
  (`cli/internal/cli/provisioning.go`) gives the console and `ls` the feature
  names alone.
- **The steps that take time say nothing a machine can read.**
  - Claude Code's `cli` step is `install-cli.sh`, which runs
    `npm-global install @anthropic-ai/claude-code`
    (`sandbox-agent/features/node/npm-global`). npm has no TTY there, so it
    prints no progress, and it logs a fetch only once the fetch is done. On a
    slow link, the package's one large tarball is the whole wait.
  - The node feature's `install-node.sh` runs NodeSource's setup script and
    `apt-get install`. The `packages` step runs `apt-get` itself
    (`applyPackages`, `sandbox-agent/provision/steps.go`). apt can report
    machine-readable progress on a file descriptor (`APT::Status-Fd`), but
    nothing asks it to.
  - The `download` step (`applyDownload`, `steps.go`) copies the response
    body with `io.Copy` and knows its `Content-Length`, but counts nothing.
- **A change reaches the CLI within seconds.** A change to the tracker moves
  the agent's status wait (`WaitSandboxAgentStatus`,
  `sandbox-agent/server/handler.go`). The pool then reads the agent's status
  and pushes it to the server, at most once per
  `sandboxAgentStatusWakeSpacing` (2 s) per box (`pool-agent/statuswait.go`).
  Each wake also costs the box a git status of every source.

#1437 (PR #1520) gave a disco-vm pool's own bring-up named phases. That is
the pool's path (`PoolProgressReporter`). It isn't this one: every pool shows
this line, whatever its driver.

The triage named three ways a running step could say more than its name:

- **(a)** The runner publishes the step's last output line.
- **(b)** A progress channel the agent hands each step, which a step's script
  writes and the agent's own steps fill in.
- **(c)** The agent measures the bytes the box receives while a step runs.

## Decision

### 1. A running feature carries a `progress`

`FeatureStatus` gains `progress`, present only while the feature's `state` is
`provisioning`. It describes the step named by `step`, and is reset when the
step changes. It holds:

| Part | Holds | Written by |
| --- | --- | --- |
| detail | One line of what the step is doing now: `downloading @anthropic-ai/claude-code`, `installing nodejs`. | The step (§2), or the agent for its own typed steps (§3). |
| done, total | How far the step has got, in bytes, with `total` only when the step knows it. Or a percentage, when the step knows a fraction and no bytes (apt's install phase). | Same as detail. |
| received | Bytes the box received since the step started, and when that count last moved. | The agent, always (§4). |

Every part is optional except `received`. A step that reports nothing still
shows bytes arriving, or shows that nothing is arriving.

The `provision.json` shape of ADR 26-10-16-423 §6 gains this field and nothing
else. A reader that predates it ignores it. An agent that predates it writes
none, and its boxes show what they show today.

### 2. A `run` step reports on a file the agent names

The runner gives every `run` and `check` script
`DISCOBOX_PROVISION_PROGRESS`, the path of a file of the step's own, in a
directory only root can write. A script that has something to say writes a
line to it, and the agent reads the latest line. A script that ignores it is
unaffected. A script running outside the agent (a test, a person) finds the
variable unset and writes nothing.

The platform's helpers use it, so every feature built on them reports
without code of its own:

- **`npm-global install`** sets the detail to the packages it installs
  (`installing @anthropic-ai/claude-code`). npm can't report the bytes of a
  fetch in flight, so the count is §4's.
- **An apt install from a script** (the node feature's `install-node.sh`)
  goes through a platform helper that runs `apt-get` with `APT::Status-Fd`,
  as the `packages` step does (§3), and writes what it reads.

A file, not an inherited fd, because a script hands its work to programs
that close or reuse fds (`npm` and its child processes, `sudo`, `runuser`),
and a path survives those. A file also leaves the step's stdout and stderr
alone, for the log.

### 3. The agent's own steps fill in progress themselves

- **`download`** counts the body as it copies it. The `Content-Length` is
  the total when the response has one. The detail is the URL's host and last
  path segment.
- **`packages`** runs `apt-get` with `APT::Status-Fd`, and turns its
  `dlstatus` and `pmstatus` lines into a detail (`downloading 3 of 12`,
  `installing nodejs`) and apt's percentage.
- **Every other typed step** (`file`, `systemd`, `group`, `env`) is quick
  and reports none.

### 4. The agent counts what the box receives

While a feature has a step running, the agent reads the box's received-bytes
counter from the network interfaces of its own namespace (`/proc/net/dev`,
loopback excluded), and reports what was received since the step started
and when that count last moved.

The counter is the box's, not the step's. Async features run in parallel
(ADR 26-10-16-423 §5), and a person's programs run once the box is ready, so
another feature or the person's own work can add to the count. It is shown
as what the box receives, never as the step's download. Its point is the
question the issue ends on: is anything arriving at all? On every runtime,
the box's only way out is its pool (ADR 0126 §6), so its traffic crosses its
own interfaces, and the pool's cache (`assets.discobox.internal`) is counted
with the rest.

This answers the case none of the steps can: npm fetching one large tarball,
`curl` in a feature's own script, a hung connection that receives nothing.

### 5. Progress is coalesced before it moves the status wait

A step's state and name still change `provision.json` at once, as now.
`progress` doesn't: the tracker folds every progress update into the
document, and moves the status wait for one at most every 5 s per box. So a
box downloading quickly costs the pool one wake every 5 s, not one per
buffer, on top of the existing 2 s wake spacing. `provision.json` is
rewritten at that rate too. The pool's 15 s check-in carries the latest
progress whatever happens.

A snapshot build streams its status on each change (ADR 26-11-23-004 §1),
and the same coalescing applies there.

### 6. Progress is the box's word, bounded and escaped, and decides nothing

Every part of `progress` is written in the box, as every other string in
`provision.json` is (ADR 26-10-16-423 §6).

- The detail is one line, bounded (256 bytes) by the agent before it lands
  in the document. The CLI and console escape it (`cli/REVIEW.md`) as they
  escape a step's name.
- The counts are shown, never trusted. A box can claim any number. A total
  smaller than what is done is shown as no total.
- Nothing acts on `progress`: not readiness (`Tracker.AwaitReady`), not a
  reprovision or its retry (ADR 26-11-19-797), not the server's reconcile.
  Whether a step succeeded is still its exit status alone.

### 7. The CLI and the console show it as the pull line does

Each blocking feature in progress is rendered as now, followed by its
progress, using the CLI's existing byte rendering (`bytesSuffix`,
`cli/internal/cli/imageprogress.go`):

```
provisioning claude-code / cli: installing @anthropic-ai/claude-code — 18.4 MB received — waiting 1m20s
provisioning node / install: installing nodejs, 62% — waiting 30s
provisioning claude-code / cli: installing @anthropic-ai/claude-code — nothing received for 45s — waiting 3m10s
```

- A percentage appears only when the step reports a total, or its own
  percentage. `received` never has one.
- Once `received` hasn't moved for 30 s, the line says how long nothing has
  arrived, instead of the count.
- The console's and `ls`'s provisioning text (`sandboxProvisioning`) gains
  the same detail for a single feature in progress, within its row's width.

## Alternatives rejected

- **(a) Publish the step's last output line.** It costs nothing in a
  script, and catches what a script happens to print. But the step that
  started this, npm without a TTY, prints nothing until it is done. Neither
  does `curl -fsSL`. apt's text output is a stream of package names, which
  `APT::Status-Fd` gives better. The line is also whatever a program wrote:
  a progress bar's carriage returns, escapes, a URL with a token in its
  query. Today that stays in a root-only log, and this would put it in a
  report every reader of the box sees. §2 publishes only what a script
  chose to say.
- **(c) alone: count bytes and nothing else.** It shows that something is
  moving, but not what. "Downloading node 22 (35 MB)", which the issue asks
  for, needs the step's word. It is kept as the floor under (b), not
  instead of it.
- **(b) alone: only what steps report.** It leaves every script that doesn't
  report silent, including the npm fetch this issue is about, until each
  feature is changed. §4 makes every step say at least whether anything is
  arriving.
- **Count bytes per step: a network namespace, or a proxy that tags a
  step's connections.** A step's bytes would then be its own. But each step
  would need its own namespace and egress wiring inside a box that may
  itself be a container, or the pool proxy would need to know which step in
  which box opened a connection. That is much machinery for a number shown
  to a person, who can read "the box received" for what it is.
- **An inherited fd for the channel (`DISCOBOX_PROVISION_PROGRESS_FD`).**
  It needs no file, but it dies at the first program that closes fds it
  didn't open, and a script's real work is in such programs (§2).
- **Force npm's progress with a pseudo-terminal.** npm would draw its bar,
  but into the log as escape codes. The bar tracks packages, not bytes. And
  the runner would have to give every script a TTY, which changes how other
  programs behave (colour, prompts).
- **Show nothing but the elapsed time, and point at the log.** That is
  today. A person creating a box can't read the box's log until it is up,
  which is what they're waiting for.

## Deferred

- **Why the second box was slow too.** `npm-global` installs with a private
  npm cache that it deletes afterwards, so every box downloads the CLI
  again. Reusing what one box fetched belongs to #740 (faster first provisioning
  of a box), not to this ADR, which only makes the wait visible.

## Open for implementation

- The wire names and nesting of `progress` in `FeatureStatus` and in the
  OpenAPI's agent status, and whether `received` carries a start time or a
  last-moved time.
- The line format a script writes to its progress file: plain text with a
  prefix for counts, or one JSON object a line. Whether `npm-global` and the
  apt helper are a shared shell function or a subcommand of the agent's
  `discobox-provision`.
- Where the progress files live, and that they are removed when their step
  ends.
- Whether the coalescing interval of §5 is 5 s or tuned against a real slow
  link, and whether `received` is sampled on a timer or on each read.
- Which interfaces §4 counts on each runtime: a Docker pool's container,
  each disco-vm driver's guest, and the libkrun guest, so that the pool's
  own tunnels aren't counted twice.
- How the console fits the detail and count in a narrow row.

## Consequences

- `provision.json` and `provisionspec.FeatureStatus` gain `progress`, and
  the agent status in the API carries it. It is optional and absent from
  older agents.
- Every `run` script gets one more environment variable. A feature written
  outside this repository can report progress by writing a line, and loses
  nothing if it doesn't.
- `applyDownload` and `applyPackages` count what they do. `npm-global` and
  an apt helper report what they install.
- The tracker gains a coalesced progress path beside its immediate state
  changes. A box reports at most one extra wake every 5 s per step that
  makes progress.
- The CLI's preparation line and the console's provisioning text show
  progress. A snapshot build's reported progress (ADR 26-11-23-004 §2)
  carries it with no further change.
- `pkg/provisionspec`, `sandbox-agent/provision/DESIGN.md`, and the
  `discobox` skill's description of `provision.json` follow when the work
  lands.
