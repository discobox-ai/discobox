# 0068 — Services are declared execs the sandbox starts for you

- **Status**: Accepted
- **Date**: 2026-08-20

## Context

A discobox is a place work runs, and most work needs something running beside
it: the API under development, a database, a dashboard, a watcher. Today the
only way to get one up is to open a shell in the workspace and type the command,
which means it is started by hand on every boot, it is only findable by
remembering which tab it was in, and nothing but that tab's scrollback records
what it printed.

Discobot had this: a `services` directory of shell scripts, each with a name, a
description, and the port and path it served on. The port half of that is now
answered by the sandbox itself — a standing watcher reads `/proc/net/tcp{,6}`
and probes what it finds ([ADR 0046](0046-listening-ports-are-polled-and-probed-in-the-background.md)),
so a service that listens is already reported and already forwarded by the
workspace without declaring anything. The name and the description are not
derivable from anything and stay.

The sandbox already has the runtime primitive. `execs` is a persistent exec
session: a systemd transient unit per exec, a shim that outlives the command so
output can be replayed, durable transcripts
([ADR 0028](0028-exec-log-transcripts-persist-as-compressed-sqlite-rows.md)), a
status reconcile loop, and revive-in-place under a stable exec id
([ADR 0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md)).
`terminal` is the precedent for a typed layer over it: a terminal is an exec
created in harness mode, tagged in exec metadata, launched at boot by
`EnsurePrimary`. A service is the second such layer, and the question is what,
if anything, it needs that a terminal did not.

## Decision

### 1. A service is a declared exec, and the declaration lives in the repository

`.discobox/services/` holds one executable script per service, in the same
front-matter format `.discobox/hooks/` already uses:

```bash
#!/bin/bash
#---
# name: Discobox API
# description: Runs discobox-server with watchnbuild hot reload
#---
exec go tool task dev:server
```

`name` and `description` are the whole vocabulary. Everything discobot's header
also carried — `http`, `path`, `order` — is dropped: the first two are what
ADR 0046's port watcher now answers for free, and the third described a startup
ordering this does not have (decision 4).

The service's id is filename-derived and stable the way a hook's is:
`10-discobox-api.sh` is `discobox-api`, with the `NN-` ordering prefix and the
extension stripped. That prefix survives as what it always really was — a way to
make the directory listing read in a sensible order — and stops being a
dependency declaration.

It is the repository's file, not the image's or the sandbox's, because what runs
beside your work is a property of the thing being worked on. It is read from the
primary source's working tree inside the sandbox, so the file that declares a
service and the code it runs are the same checkout, versioned together, and a
service added on a branch exists on that branch and nowhere else.

**Rejected: `image.json`-declared services.** A harness image shipping services
every sandbox on it runs is a real thing to want, and this deliberately does not
do it. Nothing in the repository can then say what the sandbox is running, and
the failure mode is a service you cannot find the source of from inside the
checkout you are working in. If an image-level need shows up, it should arrive as
its own layer with its own precedence rule, not by quietly widening this
directory's meaning.

**Rejected: every source contributes services, namespaced by slug.** A sandbox
can hold several sources ([ADR 0056](0056-a-repository-declares-the-sources-it-is-worked-on-with.md)),
and letting each contribute would make ids `<slug>/<name>` and let a companion
repository start processes in your sandbox by being included in it. The primary
source is what the sandbox is *for*; the others are there to be read and built
against. Revisit if a declared companion source turns out to routinely need its
own server up.

### 2. Services are execs tagged with the service they run

A running service is an ordinary `execs` record with `metadata.serviceId` set to
the declaration's id, created with `Shell: true` and `ShellCommandLine` set to
the script path — so the script runs under the run user's login shell, with the
`PATH`, the direnv, and the profile a person typing the same command would get.

Nothing else is new. Logs, transcripts, attach, resource sampling, status
reconcile, and the audit event log all already work on an exec and now work on a
service because a service *is* one. The exec id is the service's durable
identity across restarts, as it is for a terminal (ADR 0038), so a restart keeps
the tab it is drawn in rather than replacing it.

**Rejected: a service is its own systemd unit, written to disk.** It would get
`Restart=`, ordering, and `journalctl` for free, and it would put the sandbox's
service state somewhere `execs` cannot see: two runtimes, two status models, two
log stores, and a second answer to "what is running in this sandbox" that the
exec listing does not contain. The workspace, `disco box exec ls`, the transcript
store, and the pool agent's status poll all read execs.

### 3. Services run on pipes, not a PTY

A service is a program whose output is read after the fact, not a program you
sit in front of. Pipes keep stdout and stderr as the distinct streams the shim
already frames them as, so `disco box services logs` can route them the way a
local command does and `2>/dev/null` means something.

The cost is accepted and real: a dev server that checks `isatty` will drop its
colors and may buffer differently than it does in a terminal. That is the same
thing it does under `systemd`, `docker compose`, or any CI, which is the company
this belongs in.

### 4. A service that exits stays exited

There is no restart policy, no backoff, and no supervision loop. A service that
ends is reported with its exit code and left alone.

**Rejected: `Restart=always` / restart-on-failure.** Both were considered and
declined for now. A service that fails on the first line — the usual case while
you are editing it — becomes a hot loop that churns the transcript store and
buries the one error message that explains it. Restarting also implies a desired
state to converge on, which implies persisting that state per service and
reconciling it, and none of that is earned by "run these scripts for me". The
condition for revisiting: once services are routinely long-lived enough that
people notice one has died only when their editor stops working, restart-on-
failure with a give-up state and a visible restart count is the thing to add.

### 5. Autostart is a boot-time launch, not a reconciled desired state

At sandbox boot, alongside `EnsurePrimary`, the agent discovers the directory
and starts every service in it, once. That is the whole of "autostart".

Discovery itself is re-read on every listing rather than cached at boot, so a
service file added, edited or deleted while the sandbox is up shows up in
`services ls` immediately. A newly declared service is *listed* as stopped, not
started: starting a program because a file appeared is not something a file
appearing should do.

### 6. Stopping is a first-class exec outcome, not an inferred one

`execs.Manager` gains `Stop`, which stops the unit and records `stopped` on the
exec — distinct from `Delete`, which also tears down the record and the
transcript, and distinct from `lost`, which is what a unit vanishing underneath
a live exec means.

The alternative was to infer it: an exec that ended with no exit code was
stopped on purpose. That is a guess dressed as a fact, and it is wrong for every
program killed by a signal it did not choose. One boolean on the record, set at
the one place a stop is requested, means the listing can say *stopped* rather
than *exited (unknown)* and `restart` knows the difference between a service it
is resuming and one it is retrying.

### 7. The workspace draws services in the left column, after the terminals

*(Amended twice before this decision shipped: services were first placed in the
right-hand column and moved to the left one, and were first drawn from the exec
listing alone and given a listing of their own. The reasoning below is the
amended version.)*

[ADR 0054](0054-the-workspaces-columns-are-terminals-and-shells.md) made the
workspace's two columns the two kinds of session the server records: harness
terminals on the left, every other TTY session as a tab on the right. Services
join the **left** column, after the terminals.

They are drawn from a listing of their own, polled beside the exec listing.
Drawing them from `GET /execs` alone — which is what ADR 0054's "no second
poll" would have bought — is silent about precisely the service that needs
saying: a service is an exec, so one that never started, failed at boot, or
cannot run at all has no exec to appear in it. A tab strip that only ever shows
working services is one where a broken declaration is indistinguishable from a
service nobody wrote, which is the failure decision 1 already refuses in the
listing. The client-side layout state ADR 0054 rejected is still absent: which
services have tabs, and in what order, is entirely the server's answer.

A tab is for a service whose absence would be a surprise: running, failed,
exited, or an unrunnable declaration. One stopped on purpose has none — you
know, and a pane to dismiss every time is the window nagging. A pane with no
live process to attach to draws what there is to say instead: the state, the
reason, and the last run's output, since after a crash the output is the
reason.

A service's pane opens on its transcript either way. Decision 3's pipes have a
consequence here too: a plain exec has no screen to repaint from, so attaching
to a *running* service starts at "now" and shows nothing until it next speaks.
The history is played in ahead of the live stream, tailed to what a pane can
usefully hold.

The keys never land on a service. Nobody asked for it, and it is read-only, so
focus there is focus nowhere; the workspace lands on the primary, which usually
arrives last because it waits on its harness install while a service is already
running.

The left side is what the discobox is running on your behalf — the harness
working on the code, and the code itself running — while the right side is what
you opened by hand. Grouping them that way also keeps the split honest: only
shells put a second box on screen, so a discobox with three services and no
shell still draws its terminal at the full width.

Within the column the two kinds are grouped rather than strictly aged:
`[terminals, services]`. A service usually starts *before* the terminals do —
boot launches both and a harness has files to install first — so strict age
would put services above the primary's neighbors and push them along. Services
are ordered among themselves as the repository declares them, which is what the
numeric filename prefix was for and what `disco box services ls` already shows.

Two things follow from decision 3. A service pane is read-only: it takes no keys
and sends no resize, because there is nothing at the far end reading stdin. It
also sets LNM on its emulator, because a pipe has no line discipline to have
turned the program's line feeds into carriage-return line feeds, and a terminal
reads a bare LF as "down one row, same column" — the staircase.

And a column is no longer only TTY sessions, which is the one clause of ADR 0054
§2 this widens — deliberately, and only for sessions the sandbox itself records
as services.

**Rejected: a services screen of its own**, like the harnesses screen. It draws
a better list — status, description and lifecycle verbs in one place — and it
puts the running service one screen away from the work it is running beside,
which is the wrong side of the trade for the thing you glance at. The list it
would have drawn is `disco box services ls`.

**Rejected: the right-hand column** (the first form of this decision). It read
as "everything that is not the harness you are talking to", which is true of a
service and also true of nothing else about it: a service is the discobox
running your own work, and it belongs beside the harness working on that work
rather than among the shells you opened by hand. It also made every service
split the window, so a discobox with services and no shell drew its terminal at
half width for panes nobody was typing in.

## Consequences

- `disco box services ls/start/stop/restart/logs` and the workspace's service
  tabs are two views of one thing; neither has state the other cannot see.
- The `#---` front-matter format now has two readers in two modules, so it moves
  to the root module as `frontmatter` and `hooks/parser` is rewritten onto it.
  The format was always a `.discobox/` convention rather than a hooks-internal
  detail; this is where that becomes true in the code.
- A sandbox with no `.discobox/services` directory has no services and does no
  extra work at boot — one failed `ReadDir`.
- Services are not part of a sandbox's manifest, so nothing about pool-agent,
  `sandbox.json`, or sandbox creation changes. A service is discovered by the
  sandbox, from the sandbox, after it is running.
- A service script is held to the same two rules a hook script is: a shebang on
  the first line and the executable bit. A file that fails either is reported as
  a declaration error in the listing rather than being silently skipped.
