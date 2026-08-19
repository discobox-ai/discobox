# 0055 — A delivered source settles before its sandbox runs

- **Status**: Proposed
- **Date**: 2026-08-19

## Context

A source the client delivers by push (ADR 0001, ADR 0045) is not present when
the sandbox's container is created. The order is forced: the push is proxied
through pool-agent into the sandbox's own Git repository, and that repository
only exists once pool-agent has provisioned the sandbox — which is why
`awaiting_source` sits *after* provisioning rather than before it.

Two things follow from that order, and neither was handled.

**The harness starts against an empty workspace.** The reconciler creates a
sandbox that has never run with `CreateOptions.Start` set
(`sandboxHasNeverRun`, ADR 0017 §13), and `createSandbox` parks the sandbox
only *after* `ensureSandboxCreated` returns. So the container is created,
started, and its agent waited for; the agent boots, `boot.wireSources` binds
what is still an empty repository onto the source's target, and
`server.Serve` launches the primary terminal with the sandbox's prompt as
argv. Nothing anywhere in sandbox-agent waits for a source: `cfg.Sources` is
read only for status reporting and for the default exec workdir. The client's
push lands seconds later — updating the working tree directly, since the
repository is configured `receive.denyCurrentBranch=updateInstead` — and the
resume's `git reset --hard` + `git clean -fd` then discards whatever the
harness did in the meantime.

The control plane already refuses to route an attach through until delivery
completes (`AwaitSandboxHTTPClient`), so a human never sees the empty window.
The agent process lives through it anyway.

**The project layer is never read.** ADR 0012 §7 has pool-agent read
`.discobox/project.json` from the source "at the moment it first materializes
the source", bake it into the written document, and never re-read it. For a
push-delivered source that moment is `initGitSource` — an empty repository —
so `readProjectLayer` finds nothing. The resume that actually checks the source
out returns early from `CreateSandbox` (`!drifted` → `materializePushedSources`
→ return) and never re-runs `prepareSandboxVolumes` or rewrites the document.
The result is that `.discobox/project.json` is silently ignored for every
sandbox created from a directory with no repository (always push-delivered) and
for every run against a remote server, while working for local clone-delivered
ones — a difference nothing in the contract announces.

## Decision

### 1. A sandbox holds its first harness launch until its sources are in place

`sandboxconfig.Source` gains `AwaitsDelivery`, set by pool-agent from the same
`gitSourceAwaitsPush` predicate every other delivery decision reads. It states
what the sandbox cannot see for itself: this source's content is still being
sent by the client.

When any source carries it, `terminal.Service` blocks the primary terminal's
*first* launch until the sandbox is settled. A revive never waits: the terminal
being revived could not have been created before the source was there.

This is a wait in the agent process, not in `boot`. `boot.Init` is PID 1 and
`exec`s systemd; blocking there would stop systemd from ever starting, so the
agent would never come up, and pool-agent's `waitForSandboxAgent` (30s) would
fail the create outright — turning a sandbox that should park into one that
fails before its client can push anything. The wait has to sit after the HTTP
server is serving, and it does.

### 2. The signal is pool-agent's, and it means settled, not materialized

pool-agent publishes `/etc/discobox/ready` (`SourcesReadyFileName`) in the
config volume, which the sandbox already sees read-only. It is written only
where every source is materialized *and* the document beside it is final, and
cleared while any source is still outstanding.

The sandbox deliberately does **not** gate on the per-source materialized
marker it could read for itself (`<target>/.git/discobox-materialized`). That
marker is written the instant a checkout completes — which, per decision 3, is
before pool-agent has re-read the project layer and decided whether the
container must be rebuilt. A sandbox gated on it would launch its harness
against a configuration about to be replaced, in the window between the two.

The wait is event-driven: one `stat` first, then an fsnotify watch on the
config directory, backstopped by a one-second re-check in case an event is
dropped or the volume's filesystem does not deliver host-side writes as events.
A sandbox whose config names no source awaiting delivery — every
clone-delivered one, and every sandbox created before this contract — acquires
no wait at all and stats nothing, so the common boot path is unchanged.

### 3. A delivered project layer is applied by rebuilding the container

On the resume, after materializing the pushed sources, pool-agent re-reads the
primary source's `.discobox/project.json` and compares it with the layer
recorded in the written document's `_provenance`. When they differ, it removes
the container and falls through to the ordinary create path, which re-reads the
layer, writes a document that includes it, and builds a container that runs
what the project declares.

This amends ADR 0012 §7 for push-delivered sources only: the layer is still
resolved once and baked, still never read from inside a running sandbox, and
still not re-read when new commits arrive later. What changes is *which*
materialization counts — for a source that arrives by push, it is the resume,
not the create.

Comparing against the recorded layer, rather than rebuilding whenever a layer
is found, is what makes this terminate: once the rebuilt container's document
records it, no later create sees a difference. The pending-delivery check in
front of it means a repeat create for an already-settled sandbox does no work
at all.

## Alternatives rejected

**Rewrite `sandbox.json` in place and have the agent re-read it.** The config
volume is bind-mounted as a *directory* (`wireConfig`), so a document pool-agent
rewrites is visible inside the running container immediately, and the harness
command is only read at launch — which the gate has not released yet. It would
avoid one container rebuild. Rejected because the project layer is not only the
harness command: `FilesAdd` is installed at launch, but a document also drives
what `boot` wired (volumes, groups, user) and what the container spec was built
from, and having one field's changes apply live while its neighbours' require a
rebuild is the kind of split precedence ADR 0012 exists to remove. Rebuilding
applies all of it by the same mechanism image upgrades already use, and the
sandbox's state lives in the pool-host binds, so nothing is lost.

**Create the container stopped while a source is outstanding.** `Start=false`
until delivery would remove the empty window without any in-sandbox wait. The
git route that receives the push is wrapped in `autoStart` (ADR 0017 §12), so
the first push would start the container anyway; exempting that one route
would work — the repository is served from the pool-host tree and needs no
running container — but it makes a sandbox's power state depend on which route
was called, and `waitForSandboxAgent` would have to become conditional too.
Revisit if the in-sandbox gate proves to be the wrong place for it.

**Never create the container until the source arrives.** Cleanest in principle:
the container that runs the harness would always be built after its sources
exist, and decision 3 would collapse into the ordinary create path. Rejected as
too large a change to pool-agent's create and to what a parked sandbox reports
(it would have no container, so no runtime state, for the whole delivery
window) for the problem at hand.

**Leave it and document the window.** The window is short and the control plane
already hides it from attaching clients. Rejected because the harness is handed
the prompt at launch: an agent that acts on it immediately acts on an empty
directory, and the resume then deletes its work. That is a wrong answer
produced silently, not a cosmetic race.
