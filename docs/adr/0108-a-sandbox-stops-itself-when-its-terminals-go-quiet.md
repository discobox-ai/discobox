# 0108 — A sandbox stops itself when its terminals go quiet

- **Status**: Accepted
- **Date**: 2026-09-11
- **Relates to**: [ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
  §9, which removed desired power state so that idle-stop could exist, and
  deferred "the criteria that start and stop sandboxes"; §12, the pool agent's
  auto-start latch, which is what brings a stopped sandbox back.

## Context

Nothing stops an idle sandbox. It runs until somebody stops it or its host
goes down, holding memory and a share of its pool's envelope the whole time.
ADR 0017 §9 cleared the way for an idle policy — no stored intent exists for
one to contradict — and §12 made stopping cheap to undo: the next attach, exec,
git push, or HTTP request starts the sandbox again. What was never decided is
what "idle" means.

Three facts already exist inside the sandbox, and nowhere else first-hand:

- **The window title of every terminal.** Each exec's shim runs a screen
  emulator and holds the title the program last set with OSC 0/2
  (`shimruntime/screen.go`). Coding harnesses animate that title while they
  work — a spinner glyph ahead of the session name — and leave it still while
  they wait on the user. A changing title is the harness saying, in a
  harness-neutral way, that it is busy.
- **Who is connected.** Each shim reports its live attacher count and the last
  time a client attached or typed (`execs/shim.go`). SSH sessions are execs
  ([ADR 0024](0024-ssh-is-a-control-plane-ingress-onto-execs.md)), so they are
  counted too. TCP tunnels are dialed by the sandbox-agent itself
  (`server/tcp_attach.go`).
- **What the processes inside want.** Only something running in the sandbox
  knows it is about to start a two-hour build that will not animate anything.

## Decision

### 1. The sandbox-agent decides, and powers the sandbox off

A standing loop in the sandbox-agent (a new `autostop` package, started
beside the ports watcher) evaluates activity on an interval and, when the
sandbox has been idle for the idle timeout, runs `systemctl poweroff`. systemd
stops its units, PID 1 exits, the container stops.

Nothing else changes. The pool agent sees the Docker `die` event and reports
`stopped` on the §10 channel, exactly as for any container that exited; there
is no restart policy to bring it back (§9), and the next sandbox-directed
request starts it (§12). The server and pool agent gain no code. This meets
§9's own test for an idle policy — it writes no new state anywhere.

A sandbox in configure mode does not run the policy. It exists to run one
interactive setup command for a flow the control plane drives, and its
lifetime belongs to that flow.

It is not a caller of the pool's `stop` operation, as §9 anticipated policies
would be. The sandbox-agent has no channel to the pool (`sandbox-agent`'s
boundary rules forbid calling back), and it does not need one: powering off is
the one instruction a sandbox can give itself, and its outcome is an
observation the pool already reports.

### 2. Activity is title changes, connections, leases, and boot

The sandbox's last activity is the latest of:

- **A title change on any exec.** The shim records `titleChangedAt` when the
  emulator's title callback delivers a value different from the one it holds,
  and reports it on `/status` beside `title`. It is recorded at the source
  rather than sampled by the agent, so a change between polls is not lost, and
  it survives a sandbox-agent restart because the shim outlives one. A title
  being *set* to the value it already has is not a change — shells and
  harnesses re-emit their title on redraw.
- **A connected client.** While any exec has an attacher, or any TCP tunnel is
  open, the sandbox is active *now*. There is no timeout while someone is
  connected. After the last one leaves, its last access is the activity time;
  the shim records access on detach as well as on attach and input, so the
  clock starts when the client left rather than at its last keystroke.
- **A keepalive lease** (§4).
- **The sandbox-agent's own start**, so a sandbox that was just created or
  just auto-started gets a full window before anything else has happened.

The sandbox stops once `now ≥ lastActivity + idleTimeout`. The loop re-reads
everything immediately before powering off, and logs which activity was the
latest and when, so a stop can be explained after the fact from the journal.

### 3. The idle timeout is a sandbox-agent default

`idleTimeout` defaults to 30 minutes, and the evaluation interval to 30
seconds. It is not configurable through the server, the pool, the image, or the
project. A sandbox that must not stop holds a lease (§4) instead; that covers
every case a knob would, from inside the sandbox, without a new field in any
layer of `sandbox.json`.

### 4. A lease is a file whose mtime is the time it holds the sandbox until

`/run/discobox/keepalive/` is a sticky, world-writable directory (mode 1777,
created by the sandbox-agent when the policy starts).
Every regular file in it is a lease, and its modification time counts as an
activity time — including one in the future:

```sh
touch -d '+2 hours' /run/discobox/keepalive/build   # keep alive for two hours
touch /run/discobox/keepalive/agent                 # heartbeat: active now
rm /run/discobox/keepalive/build                    # release early
```

So a lease dated `T` holds the sandbox until at least `T`, and it stops at
`T + idleTimeout` if nothing else has happened by then.

- **mtime, not contents.** `touch -d` is the entire interface, available to
  every shell and every harness with no format to get wrong, and setting a
  timestamp is atomic where writing one into a file is not.
- **A directory, not one file.** Two holders cannot shorten each other's
  lease; each names its own file, and the latest mtime wins.
- **`/run`, not the home directory.** `/run` is tmpfs, so a lease dies with
  the boot that took it. A stopped sandbox — whether stopped by this policy or
  by a user — starts clean, rather than being held by a reservation someone
  made last week.

The image's `discobox` skill — what every harness reads to orient itself in a
sandbox — says the sandbox stops itself when idle, what counts as activity, and
how and when to take a lease
([ADR 0080](0080-the-image-ships-the-skills-for-what-it-installs.md)). A lease
nobody knows about holds nothing.

### 5. The status payload reports the policy's view

`SandboxAgentStatusResponse` gains an optional `autostop` object, present
while the policy runs: `idleTimeoutSeconds`, `lastActivityAt`, `lastActivity`
(what that activity was — a title change, a client, a lease, by name),
`stopsAt`, and `leaseUntil` (the latest lease mtime, if any is in the
future). It is a contract edit in
`api/openapi/server.yaml` and nothing more — the server stores the payload
verbatim (ADR 0030), so a client can render "stops in 12m" and a stopped
sandbox's row records why without any server logic.

## Alternatives rejected

**The server reaps idle sandboxes.** It already stores the relayed status and
derives `LastActiveAt` from it, and a server-side policy could call `stop`
exactly as §9 imagined. Rejected because every fact the decision needs is
first-hand inside the sandbox and second-hand everywhere else. The lease would
need a channel up to the server and a reason to trust it; title changes would
arrive as 15-second samples; and the decision would depend on the whole
poll-mint-push chain being healthy, so a pool that lost its connection to the
server would stop reaping entirely, or reap on stale data.

**The pool agent decides.** It polls every sandbox's status already and holds
the per-sandbox mutex that serializes stop against auto-start, which would
close the race described under Consequences. Rejected for the same reason as
the server, one hop shorter: the lease still has to be relayed, and the
decision still inherits the poller's health. The race it would close costs a
retry.

**CPU or resource usage as the idleness signal.** Rejected in both directions.
Things nobody is using keep a sandbox busy — dockerd, nix-daemon, language
servers, file watchers — and a harness waiting on a model response is working
while using almost no CPU. An animating title is the harness itself saying it
is busy. [ADR 0071](0071-resource-accounting-is-a-pool-agent-differenced-report.md)
also puts rates in the pool agent, which the sandbox would have to reinvent.

**Harness hook events as the activity signal.** `agentstatus` stopped deriving
what a harness is doing from its hooks because that needed a per-harness event
mapping only claude-code ever had. Titles need no mapping; every harness that
animates one already works.

**A lease timestamp written into the file.** Needs a format, a parser, and an
answer for half-written files and ones that fail to parse. mtime has none of
those.

**A lease that holds until exactly `T` and counts for nothing after it.**
Closer to "keep me alive until `T`", but a plain `touch` would do nothing — the
first thing anyone tries — and a lease would behave differently from every
other activity. Treating the mtime as activity keeps one rule and makes the
heartbeat work.

**A configurable idle timeout now.** The obvious home is a runtime-layer field
in `sandbox.json`, set by the server, which is exactly the server work this
decision avoids. Nothing yet needs a value other than the default that a lease
cannot provide; see Deferred.

## Consequences

- A sandbox whose work does not animate a terminal title stops after the idle
  timeout: a dev server with nobody attached, a declared service
  ([ADR 0070](0070-services-are-declared-execs-the-sandbox-starts-for-you.md)),
  a build in a background shell. Those need a lease. This is the intended
  behavior — a sandbox that nobody is watching and nothing claims is idle.
- Traffic through the pool agent's `/http/{port}` route goes to the
  container's IP directly and never passes through the sandbox-agent, so
  browsing a forwarded dev server does not keep the sandbox up. The request
  that arrives after it stops auto-starts it.
- A harness sitting on a permission prompt or waiting on the user holds a
  still title and is stopped. On the next attach the terminal revives in
  place ([ADR 0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md))
  through the harness's relaunch command, which resumes its session.
- A program that animates its title forever holds the sandbox forever. That is
  the same claim a lease makes, and it is honored the same way.
- A request that reaches the sandbox between the decision and the container's
  exit gets a dropped connection: §12's latch saw `running` and forwarded it. A
  retry lands on a stopped sandbox and starts it. The window is systemd's
  shutdown time.
- The first request after a stop pays a container start (ADR 0017 §12's
  consequence, now common rather than rare).
- The server, pool agent, and CLI need no change. The API contract gains one
  optional object (§5).

## Deferred

- **A per-pool or per-project idle timeout.** Revisit when a user needs a
  timeout the default and a lease cannot express together — for example a
  cost-sensitive pool that wants five minutes. The home is a runtime-layer
  `sandbox.json` field the pool agent writes.
- **Connections to listening ports as activity.** `ports` already reads
  `/proc/net/tcp`; counting established connections to the sandbox's own
  listening sockets would make `/http/{port}` traffic keep a sandbox up.
  Revisit when a user loses a sandbox they were actively using through a
  forwarded port.
- **A CLI verb for the lease** (`discobox keepalive 2h`). `touch -d` is enough
  until a harness or user shows it is not.
- **The latch retrying a sandbox that stops mid-request.** Revisit if the
  dropped connection under Consequences shows up as a user-visible failure
  rather than a retry.
